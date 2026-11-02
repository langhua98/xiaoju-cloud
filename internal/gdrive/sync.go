package gdrive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// 同步规则（双向）：
//   - 两边对应关系记在 gdrive_items 里：谷歌的 ID ↔ 网盘的 ID，以及上次同步完时两边各自的名字、上级、内容指纹。
//   - 只有一边有、没有记录的：新建到另一边（同名文件夹直接配对；同名文件内容一样直接配对，不一样两份都留，
//     另一边那份名字后面加“(谷歌)”或“(小橘)”）。
//   - 有记录、一边没了：另一边移进回收站（文件在另一边又改过的不删，下次当新文件同步过去）。
//   - 有记录、两边都在：哪边的名字、位置变了就照着改另一边；哪边的内容变了就把它复制到另一边，两边都改了以谷歌为准，
//     网盘里那一版留一份“(小橘)”副本。网盘这边换内容是上传一个新文件、旧的进回收站（30 天内能找回）。
//   - 一次要删的超过 20 个而且超过一半，先暂停，网页上确认后再继续（防止授权出问题、文件夹被移走时把另一边清空）。
//   - 谷歌文档、表格这类在线文件和快捷方式没有可下载的内容，跳过。

const (
	refreshAAD = "xiaoju gdrive refresh token"
	// 一次最多删这么多不用确认
	massDeleteMin = 20
	// fullEvery：没有发现变化时也隔这么久整个对一遍（漏掉的变化、上次出错的）
	fullEvery = time.Hour
)

// ErrNotConfigured 表示服务端没配置 GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET。
var ErrNotConfigured = errors.New("google drive sync is not configured on the server (GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET)")

// Result 是一次同步做了什么。
type Result struct {
	Down      int      `json:"down"`      // 谷歌 → 网盘（新文件、新内容）
	Up        int      `json:"up"`        // 网盘 → 谷歌
	Folders   int      `json:"folders"`   // 新建的文件夹（两边合计）
	Renamed   int      `json:"renamed"`   // 改名、移动
	Trashed   int      `json:"trashed"`   // 移进回收站（两边合计）
	Conflicts int      `json:"conflicts"` // 两边都改了、留了副本
	Skipped   int      `json:"skipped"`   // 谷歌文档这类跳过的
	Items     int      `json:"items"`     // 同步着的条目数
	Errors    []string `json:"errors,omitempty"`
	Seconds   float64  `json:"seconds"`
}

func (r *Result) fail(format string, a ...any) {
	if len(r.Errors) < 10 {
		r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
	}
}

// Syncer 在后台同步所有配置好的文件夹。
type Syncer struct {
	OAuth     *OAuth
	svc       *service.Service
	store     *meta.Store
	userID    uuid.UUID
	masterKey []byte
	log       *slog.Logger

	run  sync.Mutex // 同一时间只同步一次
	kick chan struct{}

	mu           sync.Mutex
	running      bool
	allowDeletes map[uuid.UUID]bool // 网页上确认过“一次删很多”的同步
	lastCheck    time.Time
	lastFull     map[uuid.UUID]time.Time
}

func New(oauth *OAuth, svc *service.Service, userID uuid.UUID, masterKey []byte, log *slog.Logger) *Syncer {
	return &Syncer{
		OAuth: oauth, svc: svc, store: svc.Store(), userID: userID, masterKey: masterKey, log: log,
		kick: make(chan struct{}, 1), allowDeletes: map[uuid.UUID]bool{}, lastFull: map[uuid.UUID]time.Time{},
	}
}

// Configured 报告服务端有没有配置 OAuth 客户端。
func (s *Syncer) Configured() bool {
	return s != nil && s.OAuth != nil && s.OAuth.ClientID != "" && s.OAuth.ClientSecret != ""
}

// Running 报告现在是不是正在同步。
func (s *Syncer) Running() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Syncer) LastCheck() time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCheck
}

// Kick 让后台马上同步一次（全部整个对一遍）。
func (s *Syncer) Kick() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lastFull = map[uuid.UUID]time.Time{}
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// AllowDeletes 让暂停了的同步下次照样删（网页上确认过）。
func (s *Syncer) AllowDeletes(linkID uuid.UUID) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.allowDeletes[linkID] = true
	s.mu.Unlock()
}

// SealRefresh / OpenRefresh 用主密钥加密、解密 refresh token。
func (s *Syncer) SealRefresh(token string) ([]byte, error) {
	return crypto.Seal(s.masterKey, []byte(token), []byte(refreshAAD))
}

func (s *Syncer) client(ctx context.Context) (*Client, *meta.GDriveAccount, error) {
	if !s.Configured() {
		return nil, nil, ErrNotConfigured
	}
	acct, err := s.store.GetGDriveAccount(ctx, s.userID)
	if err != nil {
		return nil, nil, err
	}
	tok, err := crypto.Open(s.masterKey, acct.RefreshSealed, []byte(refreshAAD))
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt google token: %w", err)
	}
	return s.OAuth.Client(string(tok)), acct, nil
}

// Client 返回连接的谷歌账号的客户端（网页挑文件夹用）。
func (s *Syncer) Client(ctx context.Context) (*Client, error) {
	c, _, err := s.client(ctx)
	return c, err
}

// Run 每隔 every 看一次有没有变化，有就同步。
func (s *Syncer) Run(ctx context.Context, start, every time.Duration) {
	if !s.Configured() {
		return
	}
	timer := time.NewTimer(start)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.kick:
		}
		if err := s.SyncOnce(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, meta.ErrNotFound) {
			s.log.Warn("gdrive sync", "err", err)
		}
		timer.Reset(every)
	}
}

// SyncOnce 看一遍所有的同步：谷歌、网盘两边有变化的（或者隔了很久的）整个对一遍。
func (s *Syncer) SyncOnce(ctx context.Context) error {
	s.run.Lock()
	defer s.run.Unlock()
	s.mu.Lock()
	s.running = true
	s.lastCheck = time.Now()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	links, err := s.store.ListGDriveLinks(ctx, s.userID)
	if err != nil || len(links) == 0 {
		return err
	}
	c, acct, err := s.client(ctx)
	if err != nil {
		return err
	}
	// 谷歌那边有没有变化：changes 接口从上次的位置往后看。位置在对之前取，同步时自己做的改动下次会再对一遍（什么都不用做）。
	gChanged, newToken := true, ""
	if acct.PageToken != "" {
		gChanged, newToken, err = c.Changed(ctx, acct.PageToken)
	}
	if acct.PageToken == "" || err != nil {
		newToken, err = c.StartPageToken(ctx)
		gChanged = true
	}
	if err != nil {
		if errors.Is(err, ErrRevoked) {
			_ = s.store.SetGDriveAccountError(ctx, s.userID, "谷歌授权失效了，请重新连接谷歌云盘")
		}
		return err
	}
	if acct.Error != "" {
		_ = s.store.SetGDriveAccountError(ctx, s.userID, "")
	}

	allOK := true
	for _, l := range links {
		if l.Paused {
			continue
		}
		fp, err := s.store.SubtreeFingerprint(ctx, s.userID, l.FolderID)
		if err != nil {
			return err
		}
		s.mu.Lock()
		due := time.Since(s.lastFull[l.ID]) > fullEvery
		s.mu.Unlock()
		if !gChanged && fp == l.Fingerprint && !due {
			continue
		}
		start := time.Now()
		res, err := s.syncLink(ctx, c, l)
		res.Seconds = time.Since(start).Round(time.Millisecond).Seconds()
		msg := ""
		switch {
		case err != nil:
			msg = err.Error()
			fp = ""
			allOK = false
		case len(res.Errors) > 0:
			msg = fmt.Sprintf("有 %d 个没同步成功，下次再试：%s", len(res.Errors), res.Errors[0])
			fp = ""
			allOK = false
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errPaused) {
			continue // 已经记下暂停原因
		}
		if err := s.store.FinishGDriveSync(ctx, l.ID, res, msg, fp); err != nil {
			return err
		}
		if err == nil {
			s.mu.Lock()
			s.lastFull[l.ID] = time.Now()
			s.mu.Unlock()
		}
		changed := res.Down + res.Up + res.Folders + res.Renamed + res.Trashed + res.Conflicts
		if changed > 0 || msg != "" {
			s.log.Info("gdrive sync", "link", l.ID, "down", res.Down, "up", res.Up, "folders", res.Folders,
				"renamed", res.Renamed, "trashed", res.Trashed, "conflicts", res.Conflicts, "errors", len(res.Errors),
				"seconds", res.Seconds, "err", msg)
		}
	}
	if allOK && newToken != "" && newToken != acct.PageToken {
		return s.store.SetGDrivePageToken(ctx, s.userID, newToken)
	}
	return nil
}

var errPaused = errors.New("sync paused")

func (s *Syncer) pause(ctx context.Context, l *meta.GDriveLink, msg string) error {
	if err := s.store.SetGDriveLinkPaused(ctx, s.userID, l.ID, true, msg); err != nil {
		return err
	}
	s.log.Warn("gdrive sync paused", "link", l.ID, "reason", msg)
	return errPaused
}

// linkRun 是同步一对文件夹时的工作状态。
type linkRun struct {
	s   *Syncer
	c   *Client
	l   *meta.GDriveLink
	ctx context.Context
	res Result

	gByID  map[string]*File
	xByID  map[uuid.UUID]*meta.File
	gKids  map[string][]*File         // 谷歌文件夹 → 里面的条目
	xKids  map[uuid.UUID][]*meta.File // 网盘文件夹 → 里面的条目
	byG    map[string]*meta.GDriveItem
	byX    map[uuid.UUID]*meta.GDriveItem
	gNames map[string]map[string]bool    // 谷歌文件夹里已经用了的名字
	xNames map[uuid.UUID]map[string]bool // 网盘文件夹里已经用了的名字
	done   map[string]bool               // 这次已经处理过的（"g:"+ID / "x:"+ID）
}

func (s *Syncer) syncLink(ctx context.Context, c *Client, l *meta.GDriveLink) (Result, error) {
	r := &linkRun{s: s, c: c, l: l, ctx: ctx,
		gByID: map[string]*File{}, xByID: map[uuid.UUID]*meta.File{},
		gKids: map[string][]*File{}, xKids: map[uuid.UUID][]*meta.File{},
		byG: map[string]*meta.GDriveItem{}, byX: map[uuid.UUID]*meta.GDriveItem{},
		gNames: map[string]map[string]bool{}, xNames: map[uuid.UUID]map[string]bool{}, done: map[string]bool{}}

	root, err := s.store.Get(ctx, s.userID, l.FolderID)
	if err != nil || root.TrashedAt != nil {
		return r.res, s.pause(ctx, l, "网盘这边的文件夹不在了（进了回收站？），同步已暂停")
	}
	groot, err := c.Get(ctx, l.GDriveID)
	if IsNotFound(err) || (err == nil && groot.Trashed) {
		return r.res, s.pause(ctx, l, "谷歌云盘那边的文件夹不在了（删了或者没有权限了），同步已暂停")
	}
	if err != nil {
		return r.res, err
	}

	gTree, err := c.Tree(ctx, l.GDriveID)
	if err != nil {
		return r.res, err
	}
	xTree, err := s.store.Subtree(ctx, s.userID, l.FolderID)
	if err != nil {
		return r.res, err
	}
	items, err := s.store.ListGDriveItems(ctx, l.ID)
	if err != nil {
		return r.res, err
	}
	var gList []*File
	for _, g := range gTree {
		if g.Native() {
			r.res.Skipped++
			continue
		}
		gList = append(gList, g)
		r.gByID[g.ID] = g
		p := g.Parents[0]
		r.gKids[p] = append(r.gKids[p], g)
		r.useG(p, g.Name)
	}
	for _, x := range xTree {
		r.xByID[x.ID] = x
		p := *x.ParentID
		r.xKids[p] = append(r.xKids[p], x)
		r.useX(p, x.Name)
	}
	for _, it := range items {
		r.byG[it.GDriveID] = it
		r.byX[it.FileID] = it
	}

	if err := r.deletions(items); err != nil {
		return r.res, err
	}
	// 新的：先谷歌 → 网盘（同名的配对），再网盘 → 谷歌；都是上级在前
	for _, g := range gList {
		if r.byG[g.ID] == nil && !r.done["g:"+g.ID] {
			r.newFromG(g)
		}
	}
	for _, x := range xTree {
		if r.byX[x.ID] == nil && !r.done["x:"+x.ID.String()] {
			r.newFromX(x)
		}
	}
	// 以前就有的：改名、移动，再看内容
	for _, it := range items {
		if r.done["g:"+it.GDriveID] || r.byG[it.GDriveID] != it {
			continue
		}
		g, x := r.gByID[it.GDriveID], r.xByID[it.FileID]
		if g == nil || x == nil {
			continue
		}
		r.moved(it, g, x)
		if !it.IsFolder {
			r.content(it, g, x)
		}
	}
	r.res.Items = len(r.byG)
	return r.res, ctx.Err()
}

func (r *linkRun) useG(parent, name string) {
	if r.gNames[parent] == nil {
		r.gNames[parent] = map[string]bool{}
	}
	r.gNames[parent][name] = true
}

func (r *linkRun) useX(parent uuid.UUID, name string) {
	if r.xNames[parent] == nil {
		r.xNames[parent] = map[string]bool{}
	}
	r.xNames[parent][name] = true
}

// freeName 返回 used 里没有的名字：name、name (2)、name (3)…
func freeName(used map[string]bool, name string) string {
	if !used[name] {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		n := base + " (" + strconv.Itoa(i) + ")" + ext
		if !used[n] {
			return n
		}
	}
}

// LocalName 把谷歌的名字改成网盘里能用的（网盘的名字里不能有 / \\，最长 255 字节）。
func LocalName(name string) string {
	name = strings.NewReplacer("/", "／", "\\", "＼", "\x00", "").Replace(name)
	switch name {
	case "", ".", "..":
		name = "未命名"
	}
	for len(name) > 255 {
		r := []rune(name)
		name = string(r[:len(r)-1])
	}
	return name
}

// tagged 给名字加上来源：tagged("a.jpg", "谷歌") == "a (谷歌).jpg"。
func tagged(name, tag string) string {
	ext := filepath.Ext(name)
	if ext == name {
		ext = ""
	}
	return strings.TrimSuffix(name, ext) + " (" + tag + ")" + ext
}

// g2x / x2g：一边的文件夹对应另一边的哪个文件夹（根目录 ↔ 根目录）。
func (r *linkRun) g2x(gid string) (uuid.UUID, bool) {
	if gid == r.l.GDriveID {
		return r.l.FolderID, true
	}
	if it := r.byG[gid]; it != nil && it.IsFolder {
		return it.FileID, true
	}
	return uuid.Nil, false
}

func (r *linkRun) x2g(xid uuid.UUID) (string, bool) {
	if xid == r.l.FolderID {
		return r.l.GDriveID, true
	}
	if it := r.byX[xid]; it != nil && it.IsFolder {
		return it.GDriveID, true
	}
	return "", false
}

func (r *linkRun) record(it *meta.GDriveItem) error {
	if old := r.byX[it.FileID]; old != nil && old.GDriveID != it.GDriveID {
		delete(r.byG, old.GDriveID)
	}
	if old := r.byG[it.GDriveID]; old != nil && old.FileID != it.FileID {
		delete(r.byX, old.FileID)
	}
	if err := r.s.store.PutGDriveItem(r.ctx, r.l.ID, it); err != nil {
		return err
	}
	r.byG[it.GDriveID] = it
	r.byX[it.FileID] = it
	return nil
}

func (r *linkRun) forget(it *meta.GDriveItem) error {
	if err := r.s.store.DeleteGDriveItem(r.ctx, r.l.ID, it.GDriveID); err != nil {
		return err
	}
	delete(r.byG, it.GDriveID)
	delete(r.byX, it.FileID)
	return nil
}

func pair(g *File, x *meta.File) *meta.GDriveItem {
	return &meta.GDriveItem{GDriveID: g.ID, FileID: x.ID, IsFolder: x.Type == meta.TypeFolder,
		GName: g.Name, GParent: g.Parents[0], GMD5: g.Version(), XName: x.Name, XParent: *x.ParentID, XSHA256: x.SHA256}
}

// markG / markX：这个条目（是文件夹的话连同里面的）这次不用再管了。
func (r *linkRun) markG(g *File) {
	r.done["g:"+g.ID] = true
	for _, k := range r.gKids[g.ID] {
		r.markG(k)
	}
}

func (r *linkRun) markX(x *meta.File) {
	r.done["x:"+x.ID.String()] = true
	for _, k := range r.xKids[x.ID] {
		r.markX(k)
	}
}

// deletions 处理有记录、但一边不在了的条目：另一边也移进回收站。
func (r *linkRun) deletions(items []*meta.GDriveItem) error {
	var gone []*meta.GDriveItem
	for _, it := range items {
		if r.gByID[it.GDriveID] == nil || r.xByID[it.FileID] == nil {
			gone = append(gone, it)
		}
	}
	if len(gone) == 0 {
		return nil
	}
	if len(gone) > massDeleteMin && len(gone)*2 > len(items) {
		r.s.mu.Lock()
		ok := r.s.allowDeletes[r.l.ID]
		delete(r.s.allowDeletes, r.l.ID)
		r.s.mu.Unlock()
		if !ok {
			return r.s.pause(r.ctx, r.l, fmt.Sprintf("这次要在另一边删掉 %d 个（同步着的一共 %d 个），太多了，先暂停。确认没问题后在网页上点“继续”", len(gone), len(items)))
		}
	}
	// 上级在前：文件夹移进回收站后，里面的就不用一个个删了
	depth := func(it *meta.GDriveItem) int {
		n := 0
		if x := r.xByID[it.FileID]; x != nil {
			for p := *x.ParentID; p != r.l.FolderID; n++ {
				px := r.xByID[p]
				if px == nil {
					break
				}
				p = *px.ParentID
			}
		} else if g := r.gByID[it.GDriveID]; g != nil {
			for p := g.Parents[0]; p != r.l.GDriveID; n++ {
				pg := r.gByID[p]
				if pg == nil {
					break
				}
				p = pg.Parents[0]
			}
		}
		return n
	}
	sortStable(gone, func(a, b *meta.GDriveItem) bool { return depth(a) < depth(b) })
	for _, it := range gone {
		g, x := r.gByID[it.GDriveID], r.xByID[it.FileID]
		switch {
		case g == nil && x == nil:
		case g == nil: // 谷歌那边删了 → 网盘这边也删
			if r.done["x:"+x.ID.String()] {
				break
			}
			if x.Type == meta.TypeFile && !bytes.Equal(x.SHA256, it.XSHA256) {
				break // 网盘这边又改过，留着，下面当新文件传上去
			}
			if err := r.s.store.Trash(r.ctx, r.s.userID, x.ID); err != nil && !errors.Is(err, meta.ErrNotFound) {
				r.res.fail("移进回收站 %s：%v", x.Name, err)
				continue
			}
			r.res.Trashed++
			delete(r.xNames[*x.ParentID], x.Name)
			r.markX(x)
		case x == nil: // 网盘这边删了 → 谷歌那边也删
			if r.done["g:"+g.ID] {
				break
			}
			if !g.IsFolder() && g.Version() != it.GMD5 {
				break
			}
			if err := r.c.Trash(r.ctx, g.ID); err != nil {
				r.res.fail("谷歌云盘移进回收站 %s：%v", g.Name, err)
				continue
			}
			r.res.Trashed++
			delete(r.gNames[g.Parents[0]], g.Name)
			r.markG(g)
		}
		if err := r.forget(it); err != nil {
			return err
		}
	}
	return nil
}

func sortStable[T any](s []T, less func(a, b T) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// unpairedX 找网盘文件夹 parent 里名字是 name、类型一样、还没有对应关系的条目。
func (r *linkRun) unpairedX(parent uuid.UUID, name string, folder bool) *meta.File {
	for _, x := range r.xKids[parent] {
		if x.Name == name && (x.Type == meta.TypeFolder) == folder && r.byX[x.ID] == nil && !r.done["x:"+x.ID.String()] {
			return x
		}
	}
	return nil
}

// newFromG 谷歌那边新的条目：网盘里同一个位置有同名的就配对，没有就新建。
func (r *linkRun) newFromG(g *File) {
	xp, ok := r.g2x(g.Parents[0])
	if !ok {
		return // 上级没同步成功
	}
	ctx := r.ctx
	local := LocalName(g.Name)
	if cand := r.unpairedX(xp, local, g.IsFolder()); cand != nil {
		same := g.IsFolder()
		if !same {
			var err error
			if same, err = r.sameContent(g, cand); err != nil {
				r.res.fail("比较 %s：%v", g.Name, err)
				return
			}
		}
		if same {
			r.done["x:"+cand.ID.String()] = true
			if err := r.record(pair(g, cand)); err != nil {
				r.res.fail("%s：%v", g.Name, err)
			}
			return
		}
		// 内容不一样：两份都留。谷歌这份在网盘里叫“名字 (谷歌)”，网盘那份传到谷歌时叫“名字 (小橘)”（见 newFromX）
		r.res.Conflicts++
		x, err := r.download(g, xp, freeName(r.xNames[xp], tagged(local, "谷歌")))
		if err != nil {
			r.res.fail("下载 %s：%v", g.Name, err)
			return
		}
		if err := r.record(pair(g, x)); err != nil {
			r.res.fail("%s：%v", g.Name, err)
		}
		return
	}
	name := freeName(r.xNames[xp], local)
	var x *meta.File
	var err error
	if g.IsFolder() {
		x, err = r.s.store.CreateFolder(ctx, r.s.userID, &xp, name)
		if err == nil {
			r.useX(xp, name)
			r.res.Folders++
		}
	} else {
		x, err = r.download(g, xp, name)
	}
	if err != nil {
		r.res.fail("同步 %s 到网盘：%v", g.Name, err)
		return
	}
	if err := r.record(pair(g, x)); err != nil {
		r.res.fail("%s：%v", g.Name, err)
	}
}

// newFromX 网盘这边新的条目：在谷歌那边新建（同名的已经在 newFromG 里配对过了，剩下的同名是内容不一样的）。
func (r *linkRun) newFromX(x *meta.File) {
	gp, ok := r.x2g(*x.ParentID)
	if !ok {
		return
	}
	name := x.Name
	if r.gNames[gp][name] {
		if x.Type == meta.TypeFile {
			name = tagged(name, "小橘")
		}
		name = freeName(r.gNames[gp], name)
	}
	var g *File
	var err error
	if x.Type == meta.TypeFolder {
		g, err = r.c.CreateFolder(r.ctx, gp, name)
		if err == nil {
			r.res.Folders++
		}
	} else {
		g, err = r.upload("", gp, name, x)
	}
	if err != nil {
		r.res.fail("同步 %s 到谷歌云盘：%v", x.Name, err)
		return
	}
	r.useG(gp, name)
	if err := r.record(pair(g, x)); err != nil {
		r.res.fail("%s：%v", x.Name, err)
	}
}

// moved 照着改过名字、位置的那一边改另一边（两边都改了以谷歌为准）。
func (r *linkRun) moved(it *meta.GDriveItem, g *File, x *meta.File) {
	gChanged := g.Name != it.GName || g.Parents[0] != it.GParent
	xChanged := x.Name != it.XName || *x.ParentID != it.XParent
	switch {
	case gChanged:
		xp, ok := r.g2x(g.Parents[0])
		if !ok {
			return
		}
		name := LocalName(g.Name)
		if x.Name == name && *x.ParentID == xp {
			break
		}
		if r.xNames[xp][name] {
			name = freeName(r.xNames[xp], name)
		}
		nx, err := r.s.store.UpdateFile(r.ctx, r.s.userID, x.ID, meta.FileUpdate{Name: &name, Move: true, ParentID: &xp})
		if err != nil {
			r.res.fail("改名、移动 %s：%v", x.Name, err)
			return
		}
		r.useX(xp, name)
		*x = *nx
		r.res.Renamed++
	case xChanged:
		gp, ok := r.x2g(*x.ParentID)
		if !ok {
			return
		}
		if g.Name == x.Name && g.Parents[0] == gp {
			break
		}
		ng, err := r.c.Update(r.ctx, g.ID, x.Name, g.Parents[0], gp)
		if err != nil {
			r.res.fail("谷歌云盘改名、移动 %s：%v", g.Name, err)
			return
		}
		r.useG(gp, x.Name)
		g.Name, g.Parents = ng.Name, []string{gp}
		r.res.Renamed++
	default:
		return
	}
	it.GName, it.GParent, it.XName, it.XParent = g.Name, g.Parents[0], x.Name, *x.ParentID
	if err := r.record(it); err != nil {
		r.res.fail("%s：%v", x.Name, err)
	}
}

// content 把改过内容的那一边复制到另一边。
func (r *linkRun) content(it *meta.GDriveItem, g *File, x *meta.File) {
	gChanged := g.Version() != it.GMD5
	xChanged := !bytes.Equal(x.SHA256, it.XSHA256)
	if !gChanged && !xChanged {
		return
	}
	if gChanged && xChanged && g.SHA256 != "" && g.SHA256 == hex.EncodeToString(x.SHA256) {
		gChanged, xChanged = false, false // 两边改成一样的了
	}
	switch {
	case gChanged && xChanged:
		// 两边都改了：网盘这一版改名留着（下次当新文件传到谷歌），谷歌的那一版下载到原来的名字
		r.res.Conflicts++
		keep := freeName(r.xNames[*x.ParentID], tagged(x.Name, "小橘"))
		if _, err := r.s.store.UpdateFile(r.ctx, r.s.userID, x.ID, meta.FileUpdate{Name: &keep}); err != nil {
			r.res.fail("%s：%v", x.Name, err)
			return
		}
		r.useX(*x.ParentID, keep)
		nx, err := r.download(g, *x.ParentID, x.Name)
		if err != nil {
			r.res.fail("下载 %s：%v", g.Name, err)
			_ = r.forget(it)
			return
		}
		_ = r.record(pair(g, nx))
	case gChanged:
		nx, err := r.replaceX(g, x)
		if err != nil {
			r.res.fail("下载 %s：%v", g.Name, err)
			return
		}
		it.FileID, it.GMD5, it.XSHA256, it.XName = nx.ID, g.Version(), nx.SHA256, nx.Name
		_ = r.record(it)
	case xChanged:
		ng, err := r.upload(g.ID, g.Parents[0], g.Name, x)
		if err != nil {
			r.res.fail("上传 %s：%v", x.Name, err)
			return
		}
		it.GMD5, it.XSHA256 = ng.Version(), x.SHA256
		_ = r.record(it)
	}
}

// replaceX 网盘这边换内容：先把谷歌的新版本传成一个临时名字，再把旧的移进回收站、新的改回原来的名字。
func (r *linkRun) replaceX(g *File, x *meta.File) (*meta.File, error) {
	p := *x.ParentID
	tmp := freeName(r.xNames[p], tagged(x.Name, "同步中"))
	nx, err := r.download(g, p, tmp)
	if err != nil {
		return nil, err
	}
	if err := r.s.store.Trash(r.ctx, r.s.userID, x.ID); err != nil && !errors.Is(err, meta.ErrNotFound) {
		return nil, err
	}
	name := x.Name
	if out, err := r.s.store.UpdateFile(r.ctx, r.s.userID, nx.ID, meta.FileUpdate{Name: &name}); err == nil {
		nx = out
	}
	return nx, nil
}

// download 把谷歌的文件存进网盘文件夹 parent，名字是 name。
func (r *linkRun) download(g *File, parent uuid.UUID, name string) (*meta.File, error) {
	body, err := r.c.Download(r.ctx, g.ID)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	x, err := r.s.svc.Upload(r.ctx, service.UploadRequest{
		UserID: r.s.userID, ParentID: &parent, Name: name, Mime: g.MimeType, Body: body,
	})
	if errors.Is(err, meta.ErrConflict) {
		return nil, fmt.Errorf("网盘里已经有同名的 %s", name)
	}
	if err != nil {
		return nil, err
	}
	r.useX(parent, name)
	r.res.Down++
	return x, nil
}

// upload 把网盘的文件传到谷歌（id 为空是新建，不为空是换内容）。
func (r *linkRun) upload(id, parent, name string, x *meta.File) (*File, error) {
	_, rd, err := r.s.svc.Open(r.ctx, r.s.userID, x.ID)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	g, err := r.c.Upload(r.ctx, id, parent, name, x.Mime, io.Reader(rd), x.Size)
	if err != nil {
		return nil, err
	}
	r.res.Up++
	return g, nil
}

// sameContent 比较谷歌的文件和网盘的文件内容是不是一样：谷歌给了 SHA-256 直接比，没给就下载下来算。
func (r *linkRun) sameContent(g *File, x *meta.File) (bool, error) {
	if g.SizeBytes() != x.Size {
		return false, nil
	}
	if g.SHA256 != "" {
		return g.SHA256 == hex.EncodeToString(x.SHA256), nil
	}
	body, err := r.c.Download(r.ctx, g.ID)
	if err != nil {
		return false, err
	}
	defer body.Close()
	h := sha256.New()
	if _, err := io.Copy(h, body); err != nil {
		return false, err
	}
	return bytes.Equal(h.Sum(nil), x.SHA256), nil
}

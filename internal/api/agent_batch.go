package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// 改名、移动、移入回收站、恢复：单个（PATCH / DELETE /items/{id}、/restore）和批量（POST /batch）走同一套。
// 每个改动都在审计日志里记下改之前的名字和位置，网页上可以一键撤销。
// 一次改动超过 agentPlanFiles 个文件时要先预演（dry_run），拿到 plan_id 后 10 分钟内原样再发一次才执行。

const (
	maxBatchOps = 500
	planTTL     = 10 * time.Minute
)

var (
	errEtagMismatch   = errors.New("changed since read")
	errRootLocked     = errors.New("root folder is locked")
	errRestoreOutside = errors.New("restore would land outside the token's folder")
	errNotInTrash     = errors.New("not in the trash")
	errBadOp          = errors.New("invalid operation")
)

// batchOp 是一个改动。条目用 id 或 path 指定；移动的目标用 to（路径或 ID）或 parent_id。
type batchOp struct {
	Op       string     `json:"op"` // rename | move | trash | restore
	ID       *uuid.UUID `json:"id,omitempty"`
	Path     string     `json:"path,omitempty"`
	Name     string     `json:"name,omitempty"`
	To       string     `json:"to,omitempty"`
	ParentID *uuid.UUID `json:"parent_id,omitempty"`
	IfMatch  string     `json:"if_match,omitempty"`
}

type preparedOp struct {
	op       batchOp
	file     *meta.File
	path     string
	dest     *meta.File // 移动的目标文件夹，nil 是网盘根目录
	destPath string
	files    int64 // 影响几个文件（文件夹算里面的）
	auditID  int64
}

// prepareOp 检查一个改动能不能做（范围、权限、名字），不改任何东西。
func (s *Server) prepareOp(ctx context.Context, a *agentCall, op batchOp) (*preparedOp, error) {
	p := &preparedOp{op: op}
	var err error
	switch {
	case op.ID != nil:
		p.file, p.path, err = s.agentItem(ctx, a, *op.ID, op.Op == "restore")
	case op.Path != "" && op.Op != "restore":
		if p.file, err = s.agentPath(ctx, a, op.Path); err == nil && p.file == nil {
			err = errRootLocked
		}
		if err == nil {
			p.path = "/" + joinSegs(must(splitPath(op.Path)))
		}
	default:
		return p, errBadOp
	}
	if err != nil {
		return p, err
	}
	f := p.file
	if a.root != nil && f.ID == a.root.ID {
		return p, errRootLocked
	}
	if op.IfMatch != "" && op.IfMatch != etagOf(f) {
		return p, errEtagMismatch
	}
	if op.Op != "restore" && f.TrashedAt != nil {
		return p, meta.ErrNotFound
	}
	store := s.svc.Store()
	switch op.Op {
	case "rename":
		if validName(op.Name) != nil {
			return p, errors.New("invalid path")
		}
		p.files = 0
	case "move":
		switch {
		case op.ParentID != nil:
			p.dest, p.destPath, err = s.agentFolderArg(ctx, a, op.ParentID.String())
		case op.To != "":
			p.dest, p.destPath, err = s.agentFolderArg(ctx, a, op.To)
		default:
			return p, errBadOp
		}
		if err != nil {
			return p, err
		}
		if p.dest == nil && a.root != nil {
			p.dest = a.root
		}
		if p.files, err = store.CountFilesUnder(ctx, s.userID, f); err != nil {
			return p, err
		}
	case "trash":
		if p.files, err = store.CountFilesUnder(ctx, s.userID, f); err != nil {
			return p, err
		}
	case "restore":
		if f.TrashedAt == nil {
			return p, errNotInTrash
		}
		if a.root != nil { // 原来的文件夹不在了会放回网盘根目录，那就出了令牌的范围
			parentOK := false
			if f.ParentID != nil {
				if parent, err := store.Get(ctx, s.userID, *f.ParentID); err == nil && parent.TrashedAt == nil {
					_, _, err := s.agentItem(ctx, a, parent.ID, false)
					parentOK = err == nil
				}
			}
			if !parentOK {
				return p, errRestoreOutside
			}
		}
		p.files = 1
	default:
		return p, errBadOp
	}
	return p, nil
}

func joinSegs(segs []string) string {
	out := ""
	for i, s := range segs {
		if i > 0 {
			out += "/"
		}
		out += s
	}
	return out
}

// applyOp 做一个检查过的改动，记审计日志（带改之前的样子）。
func (s *Server) applyOp(ctx context.Context, a *agentCall, p *preparedOp) error {
	store := s.svc.Store()
	f := p.file
	before := map[string]any{"name": f.Name, "parent_id": f.ParentID}
	paths := []string{p.path}
	var out *meta.File
	var err error
	switch p.op.Op {
	case "rename":
		name := p.op.Name
		out, err = store.UpdateFile(ctx, s.userID, f.ID, meta.FileUpdate{Name: &name})
		paths = append(paths, joinPath(parentPath(p.path), name))
	case "move":
		out, err = store.UpdateFile(ctx, s.userID, f.ID, meta.FileUpdate{Move: true, ParentID: idOf(p.dest)})
		paths = append(paths, joinPath(p.destPath, f.Name))
	case "trash":
		if err = store.Trash(ctx, s.userID, f.ID); err == nil {
			out, err = store.Get(ctx, s.userID, f.ID)
		}
	case "restore":
		out, err = store.Restore(ctx, s.userID, f.ID)
	}
	if err != nil {
		s.audit(ctx, a, p.op.Op, []*meta.File{f}, paths, nil, "error", codeOf(err), 0)
		return err
	}
	detail := map[string]any{"before": before, "after": map[string]any{"name": out.Name, "parent_id": out.ParentID}, "files": p.files}
	p.auditID = s.audit(ctx, a, p.op.Op, []*meta.File{f}, paths, detail, "ok", "", 0)
	p.file = out
	return nil
}

func parentPath(p string) string {
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "/"
}

// codeOf 把错误变成一个简短的代码（批量结果、审计日志里用）。
func codeOf(err error) string {
	var plan *errPlanRequired
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errOutOfScope), errors.Is(err, meta.ErrNotFound):
		return "not_found"
	case errors.Is(err, meta.ErrConflict):
		return "name_taken"
	case errors.Is(err, meta.ErrMoveIntoSelf):
		return "move_into_self"
	case errors.Is(err, errEtagMismatch):
		return "changed"
	case errors.Is(err, errRootLocked):
		return "root_locked"
	case errors.Is(err, errRestoreOutside):
		return "restore_outside"
	case errors.Is(err, errNotInTrash):
		return "not_in_trash"
	case errors.Is(err, errBadOp):
		return "invalid_op"
	case errors.As(err, &plan):
		return "plan_required"
	case err.Error() == "invalid path":
		return "invalid_name"
	}
	return "error"
}

// batchPlan 是一次预演的结果：同一把令牌 10 分钟内原样再发、涉及的条目都没被改过，才能执行。
type batchPlan struct {
	tokenID uuid.UUID
	hash    string
	etags   map[uuid.UUID]string
	expires time.Time
}

func opsHash(ops []batchOp) string {
	b, _ := json.Marshal(ops)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type batchResult struct {
	Index int        `json:"index"`
	Op    string     `json:"op"`
	ID    *uuid.UUID `json:"id,omitempty"`
	Path  string     `json:"path,omitempty"`
	To    string     `json:"to,omitempty"`
	Files int64      `json:"files"`
	OK    bool       `json:"ok"`
	Code  string     `json:"code,omitempty"`
}

// POST /batch {"ops": [{"op":"move","id"|"path","to"}, {"op":"rename","id","name"}, {"op":"trash","path"}, …],
// "dry_run": true | "plan_id": "…"}。最多 500 个。预演返回每一步会怎样和 plan_id；执行时一步步做，某一步失败不影响别的。
func (s *Server) agentBatch(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeOrganize) {
		return
	}
	a := agentFrom(r)
	ctx := r.Context()
	var req struct {
		Ops    []batchOp `json:"ops"`
		DryRun bool      `json:"dry_run"`
		PlanID string    `json:"plan_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		agentError(w, http.StatusBadRequest, "invalid_json", "invalid JSON", "")
		return
	}
	if len(req.Ops) == 0 || len(req.Ops) > maxBatchOps {
		agentError(w, http.StatusBadRequest, "invalid_request", "send between 1 and 500 ops", "")
		return
	}
	prepared := make([]*preparedOp, len(req.Ops))
	results := make([]batchResult, len(req.Ops))
	var total, trash int64
	for i, op := range req.Ops {
		p, err := s.prepareOp(ctx, a, op)
		prepared[i] = p
		res := batchResult{Index: i, Op: op.Op, Path: p.path, To: p.destPath, Files: p.files, OK: err == nil, Code: codeOf(err)}
		if p.file != nil {
			res.ID = &p.file.ID
		}
		results[i] = res
		if err == nil {
			total += p.files
			if op.Op == "trash" {
				trash += p.files
			}
		} else {
			prepared[i] = nil
		}
	}
	hash := opsHash(req.Ops)
	st := s.agents
	if req.DryRun {
		id := uuid.NewString()
		plan := &batchPlan{tokenID: a.tok.ID, hash: hash, etags: map[uuid.UUID]string{}, expires: time.Now().Add(planTTL)}
		for _, p := range prepared {
			if p != nil {
				plan.etags[p.file.ID] = etagOf(p.file)
			}
		}
		st.mu.Lock()
		for k, p := range st.plans {
			if time.Now().After(p.expires) {
				delete(st.plans, k)
			}
		}
		st.plans[id] = plan
		st.mu.Unlock()
		s.audit(ctx, a, "batch_plan", nil, nil, map[string]any{"ops": len(req.Ops), "files": total}, "ok", "", 0)
		writeJSON(w, http.StatusOK, map[string]any{"dry_run": true, "plan_id": id, "expires_at": plan.expires.UTC(),
			"total_files": total, "results": results,
			"hint": "Nothing changed yet. To run it, send the same ops again with this plan_id (and without dry_run) within 10 minutes."})
		return
	}
	if total > agentPlanFiles {
		st.mu.Lock()
		plan := st.plans[req.PlanID]
		delete(st.plans, req.PlanID)
		st.mu.Unlock()
		ok := plan != nil && plan.tokenID == a.tok.ID && plan.hash == hash && time.Now().Before(plan.expires)
		for _, p := range prepared {
			if ok && p != nil && plan.etags[p.file.ID] != etagOf(p.file) {
				ok = false
			}
		}
		if !ok {
			if req.PlanID != "" {
				agentError(w, http.StatusConflict, "plan_stale", "the plan expired, belongs to other ops, or the items changed since the preview",
					"Preview again with dry_run:true and use the new plan_id.")
				return
			}
			writePlanRequired(w, total)
			return
		}
	}
	if trash > 0 && !s.quota(w, r, quotaTrash, trash) {
		return
	}
	batch := uuid.New()
	a.batch = &batch
	done, failed := 0, 0
	for i, p := range prepared {
		if p == nil {
			failed++
			continue
		}
		if err := s.applyOp(ctx, a, p); err != nil {
			results[i].OK, results[i].Code = false, codeOf(err)
			failed++
			continue
		}
		done++
	}
	a.batch = nil
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": batch, "done": done, "failed": failed, "results": results})
}

type place struct {
	Name     string     `json:"name"`
	ParentID *uuid.UUID `json:"parent_id"`
}

func sameParent(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

var errChangedSince = errors.New("the item was changed again after this operation, so it was left alone")

// undoEntry 撤销审计日志里的一行（网页上点「撤销」）：改名、移动放回原来的名字和位置；移入回收站的恢复；
// 恢复的再移入回收站；上传的、新建的文件夹移入回收站。之后又被改过的不动。
func (s *Server) undoEntry(ctx context.Context, e *meta.AuditEntry) error {
	if e.UndoneAt != nil {
		return meta.ErrConflict
	}
	if e.Result != "ok" || len(e.FileIDs) == 0 {
		return errBadOp
	}
	store := s.svc.Store()
	var detail struct {
		Before, After *place
	}
	json.Unmarshal(e.Detail, &detail)
	id := e.FileIDs[0]
	var err error
	switch e.Action {
	case "rename", "move":
		f, gerr := store.Get(ctx, s.userID, id)
		if gerr != nil {
			return gerr
		}
		// 之后又被改了名字或挪了地方的不动（进过回收站又放回来的不算）
		if detail.Before == nil || detail.After == nil || f.TrashedAt != nil || f.Name != detail.After.Name || !sameParent(f.ParentID, detail.After.ParentID) {
			return errChangedSince
		}
		name := detail.Before.Name
		_, err = store.UpdateFile(ctx, s.userID, id, meta.FileUpdate{Name: &name, Move: true, ParentID: detail.Before.ParentID})
	case "trash":
		_, err = store.Restore(ctx, s.userID, id)
	case "restore":
		err = store.Trash(ctx, s.userID, id)
	case "upload", "mkdir":
		for _, fid := range e.FileIDs {
			if terr := store.Trash(ctx, s.userID, fid); terr != nil && !errors.Is(terr, meta.ErrNotFound) {
				err = terr
			}
		}
	default:
		return errBadOp
	}
	if err != nil {
		return err
	}
	return store.MarkAuditUndone(ctx, s.userID, e.ID)
}

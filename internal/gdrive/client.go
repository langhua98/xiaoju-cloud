// Package gdrive 把网盘里的一个文件夹和谷歌云盘里的一个文件夹双向同步。
//
// 授权：网页上点「连接谷歌云盘」→ 谷歌的授权页 → 回到服务端 /gdrive/callback，拿到 refresh token，用主密钥加密存进数据库。
// 同步：后台每隔一会儿看两边有没有变化（谷歌用 changes 接口，网盘这边看文件夹里条目的个数和最后修改时间），
// 有变化就把两边的目录树和上次同步完时记下的对应关系（gdrive_items）对一遍，见 sync.go。
// 只用 REST 接口和 net/http，不引入谷歌的 SDK。
package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	FolderMime = "application/vnd.google-apps.folder"
	// Scope：要能看到、改动用户自己建的文件夹，只能要整个云盘的权限（drive.file 只看得到本应用建的文件）。
	Scope = "https://www.googleapis.com/auth/drive"

	fileFields = "id,name,mimeType,md5Checksum,sha256Checksum,size,modifiedTime,parents,trashed"
)

// Endpoints 是谷歌接口的地址，测试时换成假的服务器。
type Endpoints struct {
	Auth   string // https://accounts.google.com/o/oauth2/v2/auth
	Token  string // https://oauth2.googleapis.com/token
	API    string // https://www.googleapis.com/drive/v3
	Upload string // https://www.googleapis.com/upload/drive/v3
}

var Google = Endpoints{
	Auth:   "https://accounts.google.com/o/oauth2/v2/auth",
	Token:  "https://oauth2.googleapis.com/token",
	API:    "https://www.googleapis.com/drive/v3",
	Upload: "https://www.googleapis.com/upload/drive/v3",
}

// OAuth 是 Google Cloud 里建的 OAuth 客户端。
type OAuth struct {
	ClientID     string
	ClientSecret string
	Endpoints    Endpoints
	HTTP         *http.Client
}

func (o *OAuth) http() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return http.DefaultClient
}

// AuthURL 是让用户点同意的谷歌授权页。prompt=consent 保证每次都给 refresh token。
func (o *OAuth) AuthURL(redirect, state string) string {
	v := url.Values{
		"client_id":     {o.ClientID},
		"redirect_uri":  {redirect},
		"response_type": {"code"},
		"scope":         {Scope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return o.Endpoints.Auth + "?" + v.Encode()
}

// ErrRevoked 表示授权失效（用户在谷歌账号里取消了授权、密码改了、测试状态的应用 7 天到期等），要重新连接。
var ErrRevoked = errors.New("google authorization expired or was revoked; reconnect Google Drive")

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func (o *OAuth) token(ctx context.Context, form url.Values) (*tokenResponse, error) {
	form.Set("client_id", o.ClientID)
	form.Set("client_secret", o.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Endpoints.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var t tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return nil, fmt.Errorf("google token: HTTP %d", resp.StatusCode)
	}
	if t.Error == "invalid_grant" {
		return nil, ErrRevoked
	}
	if resp.StatusCode != http.StatusOK || t.AccessToken == "" {
		return nil, fmt.Errorf("google token: HTTP %d %s %s", resp.StatusCode, t.Error, t.Description)
	}
	return &t, nil
}

// Exchange 用授权页回来的 code 换 refresh token。
func (o *OAuth) Exchange(ctx context.Context, code, redirect string) (refresh string, err error) {
	t, err := o.token(ctx, url.Values{"code": {code}, "redirect_uri": {redirect}, "grant_type": {"authorization_code"}})
	if err != nil {
		return "", err
	}
	if t.RefreshToken == "" {
		return "", errors.New("google did not return a refresh token")
	}
	return t.RefreshToken, nil
}

// Client 调谷歌云盘接口，access token 过期前自动用 refresh token 换新的。
type Client struct {
	oauth   *OAuth
	refresh string

	mu      sync.Mutex
	access  string
	expires time.Time
}

func (o *OAuth) Client(refresh string) *Client { return &Client{oauth: o, refresh: refresh} }

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.access != "" && time.Now().Before(c.expires) {
		return c.access, nil
	}
	t, err := c.oauth.token(ctx, url.Values{"refresh_token": {c.refresh}, "grant_type": {"refresh_token"}})
	if err != nil {
		return "", err
	}
	c.access = t.AccessToken
	c.expires = time.Now().Add(time.Duration(t.ExpiresIn)*time.Second - time.Minute)
	return c.access, nil
}

// APIError 是谷歌接口返回的错误。
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("google drive: HTTP %d: %s", e.Status, e.Message)
}

// IsNotFound 报告错误是不是“文件不存在”（被永久删除了、没有权限了）。
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// do 发请求；body 可以是 nil、[]byte（JSON）或 io.Reader（size 是长度）。限流（429、5xx）时等一会儿再试几次（body 是 io.Reader 时不重试）。
func (c *Client) do(ctx context.Context, method, u string, body any, size int64, header http.Header) (*http.Response, error) {
	delays := []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}
	for attempt := 0; ; attempt++ {
		tok, err := c.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		var rd io.Reader
		retryable := true
		switch b := body.(type) {
		case nil:
		case []byte:
			rd = bytes.NewReader(b)
		case io.Reader:
			rd = b
			retryable = false
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if _, ok := body.([]byte); ok {
			req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		}
		if _, ok := body.(io.Reader); ok {
			req.ContentLength = size
		}
		resp, err := c.oauth.http().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 300 {
			return resp, nil
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.mu.Lock()
			c.access = "" // 换一个 access token 再试
			c.mu.Unlock()
			continue
		}
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 || rateLimited(msg)) && retryable && attempt < len(delays) {
			select {
			case <-time.After(delays[attempt]):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(msg, &e)
		if e.Error.Message == "" {
			e.Error.Message = strings.TrimSpace(string(msg))
		}
		return nil, &APIError{Status: resp.StatusCode, Message: e.Error.Message}
	}
}

func rateLimited(msg []byte) bool {
	return bytes.Contains(msg, []byte("rateLimitExceeded")) || bytes.Contains(msg, []byte("userRateLimitExceeded"))
}

func (c *Client) getJSON(ctx context.Context, method, u string, body any, out any) error {
	resp, err := c.do(ctx, method, u, body, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// File 是谷歌云盘里的一个文件或文件夹。
type File struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	MimeType string    `json:"mimeType"`
	MD5      string    `json:"md5Checksum"`
	SHA256   string    `json:"sha256Checksum"`
	Size     string    `json:"size"`
	Modified time.Time `json:"modifiedTime"`
	Parents  []string  `json:"parents"`
	Trashed  bool      `json:"trashed"`
}

func (f *File) IsFolder() bool { return f.MimeType == FolderMime }

// Native 报告是不是谷歌文档、表格这类在线文件（没有可以下载的内容）或快捷方式，同步时跳过。
func (f *File) Native() bool {
	return strings.HasPrefix(f.MimeType, "application/vnd.google-apps.") && !f.IsFolder()
}

func (f *File) SizeBytes() int64 {
	n, _ := strconv.ParseInt(f.Size, 10, 64)
	return n
}

// Version 是内容的指纹：有 MD5 用 MD5，没有时用大小 + 修改时间。
func (f *File) Version() string {
	if f.MD5 != "" {
		return f.MD5
	}
	return f.Size + "@" + f.Modified.UTC().Format(time.RFC3339Nano)
}

func (c *Client) api(path string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("supportsAllDrives", "true")
	return c.oauth.Endpoints.API + path + "?" + q.Encode()
}

// Email 返回授权的谷歌账号。
func (c *Client) Email(ctx context.Context) (string, error) {
	var out struct {
		User struct {
			Email string `json:"emailAddress"`
		} `json:"user"`
	}
	err := c.getJSON(ctx, http.MethodGet, c.oauth.Endpoints.API+"/about?fields=user(emailAddress)", nil, &out)
	return out.User.Email, err
}

func (c *Client) Get(ctx context.Context, id string) (*File, error) {
	var f File
	err := c.getJSON(ctx, http.MethodGet, c.api("/files/"+url.PathEscape(id), url.Values{"fields": {fileFields}}), nil, &f)
	return &f, err
}

// List 列出文件夹里不在回收站里的条目。
func (c *Client) List(ctx context.Context, folder string) ([]*File, error) {
	return c.query(ctx, fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(folder, "'", `\'`)))
}

// Folders 列出文件夹里的子文件夹（网页上挑要同步的文件夹用）。
func (c *Client) Folders(ctx context.Context, folder string) ([]*File, error) {
	return c.query(ctx, fmt.Sprintf("'%s' in parents and trashed = false and mimeType = '%s'", strings.ReplaceAll(folder, "'", `\'`), FolderMime))
}

func (c *Client) query(ctx context.Context, q string) ([]*File, error) {
	var out []*File
	page := ""
	for {
		v := url.Values{"q": {q}, "fields": {"nextPageToken,files(" + fileFields + ")"}, "pageSize": {"1000"},
			"includeItemsFromAllDrives": {"true"}, "orderBy": {"folder,name"}}
		if page != "" {
			v.Set("pageToken", page)
		}
		var r struct {
			Files []*File `json:"files"`
			Next  string  `json:"nextPageToken"`
		}
		if err := c.getJSON(ctx, http.MethodGet, c.api("/files", v), nil, &r); err != nil {
			return nil, err
		}
		out = append(out, r.Files...)
		if page = r.Next; page == "" {
			return out, nil
		}
	}
}

// Tree 列出文件夹下面（不含它自己）的所有条目，上级在前。
func (c *Client) Tree(ctx context.Context, root string) ([]*File, error) {
	var out []*File
	queue := []string{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		kids, err := c.List(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			k.Parents = []string{id} // 同一个文件放在多个文件夹里时，以在这棵树里的位置为准
			out = append(out, k)
			if k.IsFolder() {
				queue = append(queue, k.ID)
			}
		}
	}
	return out, nil
}

// Download 下载文件内容。
func (c *Client) Download(ctx context.Context, id string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, c.api("/files/"+url.PathEscape(id), url.Values{"alt": {"media"}}), nil, 0, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *Client) CreateFolder(ctx context.Context, parent, name string) (*File, error) {
	b, _ := json.Marshal(map[string]any{"name": name, "mimeType": FolderMime, "parents": []string{parent}})
	var f File
	err := c.getJSON(ctx, http.MethodPost, c.api("/files", url.Values{"fields": {fileFields}}), b, &f)
	f.Parents = []string{parent}
	return &f, err
}

// Upload 新建文件（id 为空）或换掉已有文件的内容（id 不为空），用可续传上传：先要一个上传地址，再一次 PUT 整个内容。
func (c *Client) Upload(ctx context.Context, id, parent, name, mime string, body io.Reader, size int64) (*File, error) {
	meta := map[string]any{"name": name}
	method, path := http.MethodPost, "/files"
	if id == "" {
		meta["parents"] = []string{parent}
	} else {
		method, path = http.MethodPatch, "/files/"+url.PathEscape(id)
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	b, _ := json.Marshal(meta)
	q := url.Values{"uploadType": {"resumable"}, "supportsAllDrives": {"true"}, "fields": {fileFields}}
	h := http.Header{"X-Upload-Content-Type": {mime}, "X-Upload-Content-Length": {strconv.FormatInt(size, 10)}}
	resp, err := c.do(ctx, method, c.oauth.Endpoints.Upload+path+"?"+q.Encode(), b, 0, h)
	if err != nil {
		return nil, err
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if loc == "" {
		return nil, errors.New("google drive: no upload location")
	}
	resp, err = c.do(ctx, http.MethodPut, loc, io.Reader(body), size, http.Header{"Content-Type": {mime}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var f File
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		return nil, err
	}
	if parent != "" {
		f.Parents = []string{parent}
	}
	return &f, nil
}

// Update 改名、移动（from 不为空时从 from 移到 to）。
func (c *Client) Update(ctx context.Context, id, name, from, to string) (*File, error) {
	q := url.Values{"fields": {fileFields}}
	if from != "" && from != to {
		q.Set("addParents", to)
		q.Set("removeParents", from)
	}
	b, _ := json.Marshal(map[string]any{"name": name})
	var f File
	err := c.getJSON(ctx, http.MethodPatch, c.api("/files/"+url.PathEscape(id), q), b, &f)
	return &f, err
}

// Trash 把文件移进谷歌云盘的回收站（30 天内能找回）。
func (c *Client) Trash(ctx context.Context, id string) error {
	b, _ := json.Marshal(map[string]any{"trashed": true})
	err := c.getJSON(ctx, http.MethodPatch, c.api("/files/"+url.PathEscape(id), url.Values{"fields": {"id"}}), b, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// StartPageToken 是 changes 接口现在的位置。
func (c *Client) StartPageToken(ctx context.Context) (string, error) {
	var r struct {
		Token string `json:"startPageToken"`
	}
	err := c.getJSON(ctx, http.MethodGet, c.api("/changes/startPageToken", nil), nil, &r)
	return r.Token, err
}

// Changed 报告从 token 以后谷歌云盘里有没有任何变化，并返回新的位置。
func (c *Client) Changed(ctx context.Context, token string) (bool, string, error) {
	changed := false
	for {
		v := url.Values{"pageToken": {token}, "fields": {"nextPageToken,newStartPageToken,changes(fileId)"},
			"pageSize": {"1000"}, "includeItemsFromAllDrives": {"true"}}
		var r struct {
			Next     string `json:"nextPageToken"`
			NewStart string `json:"newStartPageToken"`
			Changes  []struct {
				FileID string `json:"fileId"`
			} `json:"changes"`
		}
		if err := c.getJSON(ctx, http.MethodGet, c.api("/changes", v), nil, &r); err != nil {
			return false, "", err
		}
		if len(r.Changes) > 0 {
			changed = true
		}
		if r.NewStart != "" {
			return changed, r.NewStart, nil
		}
		if r.Next == "" {
			return changed, token, nil
		}
		token = r.Next
	}
}

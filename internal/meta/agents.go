package meta

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AI agent 管理网盘用到的表（迁移 0016）：令牌 agent_tokens、每天的用量 agent_usage、审计日志 agent_audit、
// 危险操作申请 agent_requests。权限怎么判断在 internal/api/agent*.go，这里只管存取。

// AgentToken 是一把 agent 令牌（kind = 'agent'）。令牌明文不存，只存 SHA-256。
type AgentToken struct {
	ID           uuid.UUID        `json:"id"`
	UserID       uuid.UUID        `json:"-"`
	Kind         string           `json:"kind"`
	Name         string           `json:"name"`
	Last4        string           `json:"last4"`
	Scopes       []string         `json:"scopes"`
	RootFolderID *uuid.UUID       `json:"root_folder_id"`
	AllowPrivate bool             `json:"allow_private"`
	Limits       map[string]int64 `json:"limits"`
	IssuedUnder  string           `json:"-"`
	ExpiresAt    *time.Time       `json:"expires_at"`
	RevokedAt    *time.Time       `json:"revoked_at"`
	FrozenAt     *time.Time       `json:"frozen_at"`
	Uploads      int64            `json:"uploads"`
	Bytes        int64            `json:"bytes"`
	LastUsedAt   *time.Time       `json:"last_used_at"`
	LastIP       string           `json:"last_ip"`
	IPs          []string         `json:"ips"`
	CreatedAt    time.Time        `json:"created_at"`
}

const agentColumns = `id, user_id, kind, name, last4, scopes, root_folder_id, allow_private, limits, issued_under,
	expires_at, revoked_at, frozen_at, uploads, bytes, last_used_at, last_ip, ips, created_at`

func scanAgent(row pgx.Row) (*AgentToken, error) {
	var t AgentToken
	var limits []byte
	err := row.Scan(&t.ID, &t.UserID, &t.Kind, &t.Name, &t.Last4, &t.Scopes, &t.RootFolderID, &t.AllowPrivate, &limits, &t.IssuedUnder,
		&t.ExpiresAt, &t.RevokedAt, &t.FrozenAt, &t.Uploads, &t.Bytes, &t.LastUsedAt, &t.LastIP, &t.IPs, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Limits = map[string]int64{}
	if len(limits) > 0 {
		if err := json.Unmarshal(limits, &t.Limits); err != nil {
			return nil, err
		}
	}
	return &t, nil
}

func collectAgents(rows pgx.Rows, err error) ([]*AgentToken, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentToken{}
	for rows.Next() {
		t, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateAgentToken 存一把新的 agent 令牌（hash 是令牌的 SHA-256）。根文件夹必须是用户的、没在回收站里的文件夹。
func (s *Store) CreateAgentToken(ctx context.Context, t *AgentToken, hash []byte) (*AgentToken, error) {
	if err := s.checkParent(ctx, t.UserID, t.RootFolderID); err != nil {
		return nil, err
	}
	limits, err := json.Marshal(t.Limits)
	if err != nil {
		return nil, err
	}
	return scanAgent(s.db.QueryRow(ctx, `
		INSERT INTO agent_tokens (user_id, kind, name, token_hash, last4, scopes, root_folder_id, allow_private, limits, issued_under, expires_at)
		VALUES ($1, 'agent', $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+agentColumns,
		t.UserID, t.Name, hash, t.Last4, t.Scopes, t.RootFolderID, t.AllowPrivate, limits, t.IssuedUnder, t.ExpiresAt))
}

// AgentTokenByHash 按令牌的 SHA-256 找 agent 令牌（不管过没过期、撤销没有，调用方判断）。
func (s *Store) AgentTokenByHash(ctx context.Context, hash []byte) (*AgentToken, error) {
	return scanAgent(s.db.QueryRow(ctx, `SELECT `+agentColumns+` FROM agent_tokens WHERE token_hash = $1 AND kind = 'agent'`, hash))
}

// GetAgentToken 返回用户的一把 agent 令牌。
func (s *Store) GetAgentToken(ctx context.Context, userID, id uuid.UUID) (*AgentToken, error) {
	return scanAgent(s.db.QueryRow(ctx, `SELECT `+agentColumns+` FROM agent_tokens WHERE id = $1 AND user_id = $2 AND kind = 'agent'`, id, userID))
}

// ListAgentTokens 列出用户的 agent 令牌（包括撤销了的），最近建的在前。
func (s *Store) ListAgentTokens(ctx context.Context, userID uuid.UUID) ([]*AgentToken, error) {
	return collectAgents(s.db.Query(ctx, `
		SELECT `+agentColumns+` FROM agent_tokens WHERE user_id = $1 AND kind = 'agent'
		ORDER BY revoked_at IS NOT NULL, created_at DESC`, userID))
}

// AgentTokenUpdate 是网页上能改的地方，nil 表示不改。
type AgentTokenUpdate struct {
	Name      *string
	ExpiresAt *time.Time
	Limits    map[string]int64
	Unfreeze  bool
	// NeverExpires 把有效期改成永久（优先于 ExpiresAt）。
	NeverExpires bool
}

// UpdateAgentToken 改名字、续期、改额度、解冻。撤销了的不能改。
func (s *Store) UpdateAgentToken(ctx context.Context, userID, id uuid.UUID, u AgentTokenUpdate) (*AgentToken, error) {
	var limits []byte
	if u.Limits != nil {
		var err error
		if limits, err = json.Marshal(u.Limits); err != nil {
			return nil, err
		}
	}
	return scanAgent(s.db.QueryRow(ctx, `
		UPDATE agent_tokens SET name = COALESCE($3, name), expires_at = CASE WHEN $7 THEN NULL ELSE COALESCE($4, expires_at) END,
		       limits = COALESCE($5::jsonb, limits), frozen_at = CASE WHEN $6 THEN NULL ELSE frozen_at END
		WHERE id = $1 AND user_id = $2 AND kind = 'agent' AND revoked_at IS NULL
		RETURNING `+agentColumns, id, userID, u.Name, u.ExpiresAt, limits, u.Unfreeze, u.NeverExpires))
}

// RevokeAgentToken 撤销一把 agent 令牌（立即失效；记录留着，审计日志里还看得到名字）。
func (s *Store) RevokeAgentToken(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE agent_tokens SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND kind = 'agent' AND revoked_at IS NULL`, id, userID)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	return err
}

// RevokeAllAgentTokens 撤销用户所有的 agent 令牌，返回撤销了几把。上传链接不动（它们在网页上单独撤销）。
func (s *Store) RevokeAllAgentTokens(ctx context.Context, userID uuid.UUID) (int64, error) {
	tag, err := s.db.Exec(ctx, `UPDATE agent_tokens SET revoked_at = now() WHERE user_id = $1 AND kind = 'agent' AND revoked_at IS NULL`, userID)
	return tag.RowsAffected(), err
}

// FreezeAgentToken 冻结一把令牌（连续被拒太多次），网页上解冻。
func (s *Store) FreezeAgentToken(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `UPDATE agent_tokens SET frozen_at = now() WHERE id = $1 AND frozen_at IS NULL`, id)
	return err
}

// TouchAgentToken 记下最近一次使用的时间和 IP。这个 IP 以前没用过时返回 true（最多记 20 个）。
func (s *Store) TouchAgentToken(ctx context.Context, id uuid.UUID, ip string) (bool, error) {
	var isNew bool
	err := s.db.QueryRow(ctx, `
		WITH old AS (SELECT ips FROM agent_tokens WHERE id = $1)
		UPDATE agent_tokens t SET last_used_at = now(), last_ip = $2,
		       ips = CASE WHEN $2 = ANY(old.ips) OR $2 = '' THEN old.ips
		                  ELSE (old.ips || $2::text)[greatest(1, cardinality(old.ips) - 18):] END
		FROM old WHERE t.id = $1
		RETURNING NOT ($2 = ANY(old.ips)) AND $2 <> ''`, id, ip).Scan(&isNew)
	return isNew, err
}

// AddAgentUpload 记一次上传（个数、字节数）。
func (s *Store) AddAgentUpload(ctx context.Context, id uuid.UUID, size int64) error {
	_, err := s.db.Exec(ctx, `UPDATE agent_tokens SET uploads = uploads + 1, bytes = bytes + $2 WHERE id = $1`, id, size)
	return err
}

// AddAgentUsage 给今天（UTC）的某项用量加 n，返回加了之后的总数。
func (s *Store) AddAgentUsage(ctx context.Context, id uuid.UUID, counter string, n int64) (int64, error) {
	var total int64
	err := s.db.QueryRow(ctx, `
		INSERT INTO agent_usage (token_id, day, counter, n) VALUES ($1, (now() AT TIME ZONE 'UTC')::date, $2, $3)
		ON CONFLICT (token_id, day, counter) DO UPDATE SET n = agent_usage.n + EXCLUDED.n
		RETURNING n`, id, counter, n).Scan(&total)
	return total, err
}

// AgentUsageToday 返回今天（UTC）每项用了多少。
func (s *Store) AgentUsageToday(ctx context.Context, id uuid.UUID) (map[string]int64, error) {
	rows, err := s.db.Query(ctx, `SELECT counter, n FROM agent_usage WHERE token_id = $1 AND day = (now() AT TIME ZONE 'UTC')::date`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var c string
		var n int64
		if err := rows.Scan(&c, &n); err != nil {
			return nil, err
		}
		out[c] = n
	}
	return out, rows.Err()
}

// AuditEntry 是审计日志的一行。
type AuditEntry struct {
	ID        int64           `json:"id"`
	TokenID   *uuid.UUID      `json:"token_id"`
	TokenName string          `json:"token_name"`
	At        time.Time       `json:"at"`
	Action    string          `json:"action"`
	FileIDs   []uuid.UUID     `json:"file_ids"`
	Paths     []string        `json:"paths"`
	Detail    json.RawMessage `json:"detail"`
	Result    string          `json:"result"`
	Code      string          `json:"code,omitempty"`
	Bytes     int64           `json:"bytes,omitempty"`
	IP        string          `json:"ip"`
	UserAgent string          `json:"user_agent"`
	BatchID   *uuid.UUID      `json:"batch_id,omitempty"`
	UndoneAt  *time.Time      `json:"undone_at,omitempty"`
}

const auditColumns = `id, token_id, token_name, at, action, file_ids, paths, detail, result, code, bytes, ip, user_agent, batch_id, undone_at`

func scanAudit(row pgx.Row) (*AuditEntry, error) {
	var e AuditEntry
	err := row.Scan(&e.ID, &e.TokenID, &e.TokenName, &e.At, &e.Action, &e.FileIDs, &e.Paths, &e.Detail, &e.Result, &e.Code,
		&e.Bytes, &e.IP, &e.UserAgent, &e.BatchID, &e.UndoneAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &e, err
}

// AddAudit 记一行审计日志。
func (s *Store) AddAudit(ctx context.Context, userID uuid.UUID, e *AuditEntry) (int64, error) {
	if e.FileIDs == nil {
		e.FileIDs = []uuid.UUID{}
	}
	if e.Paths == nil {
		e.Paths = []string{}
	}
	if len(e.Detail) == 0 {
		e.Detail = json.RawMessage(`{}`)
	}
	var id int64
	err := s.db.QueryRow(ctx, `
		INSERT INTO agent_audit (user_id, token_id, token_name, action, file_ids, paths, detail, result, code, bytes, ip, user_agent, batch_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
		userID, e.TokenID, e.TokenName, e.Action, e.FileIDs, e.Paths, e.Detail, e.Result, e.Code, e.Bytes, e.IP, e.UserAgent, e.BatchID).Scan(&id)
	return id, err
}

// AuditFilter 筛选审计日志；Before 是上一页最后一行的 ID（翻页）。
type AuditFilter struct {
	TokenID *uuid.UUID
	Action  string
	Result  string
	Before  int64
	Limit   int
	Quiet   bool // 去掉成功的只读请求（列目录、搜索、缩略图…），只看改动、下载、被拒绝的
}

// quietActions 是 Quiet 时隐藏的只读动作（成功时）。
var quietActions = []string{"read", "search", "stats", "duplicates", "thumbnail", "preview", "trash_list", "batch_plan"}

// ListAudit 列出审计日志，最新的在前。
func (s *Store) ListAudit(ctx context.Context, userID uuid.UUID, f AuditFilter) ([]*AuditEntry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := s.db.Query(ctx, `
		SELECT `+auditColumns+` FROM agent_audit
		WHERE user_id = $1 AND ($2::uuid IS NULL OR token_id = $2) AND ($3 = '' OR action = $3)
		  AND ($4 = '' OR result = $4) AND ($5 = 0 OR id < $5)
		  AND (NOT $7 OR result <> 'ok' OR action <> ALL($8))
		ORDER BY id DESC LIMIT $6`, userID, f.TokenID, f.Action, f.Result, f.Before, f.Limit, f.Quiet, quietActions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AuditEntry{}
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetAudit 返回一行审计日志。
func (s *Store) GetAudit(ctx context.Context, userID uuid.UUID, id int64) (*AuditEntry, error) {
	return scanAudit(s.db.QueryRow(ctx, `SELECT `+auditColumns+` FROM agent_audit WHERE id = $1 AND user_id = $2`, id, userID))
}

// AuditBatch 返回同一次批量操作里成功的那些行，后做的在前（撤销时倒着来）。
func (s *Store) AuditBatch(ctx context.Context, userID, batch uuid.UUID) ([]*AuditEntry, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+auditColumns+` FROM agent_audit WHERE user_id = $1 AND batch_id = $2 AND result = 'ok' ORDER BY id DESC`, userID, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AuditEntry{}
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkAuditUndone 记下这一行已经撤销过了（只能撤销一次）。已经撤销过时返回 ErrConflict。
func (s *Store) MarkAuditUndone(ctx context.Context, userID uuid.UUID, id int64) error {
	tag, err := s.db.Exec(ctx, `UPDATE agent_audit SET undone_at = now() WHERE id = $1 AND user_id = $2 AND undone_at IS NULL`, id, userID)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrConflict
	}
	return err
}

// PurgeOldAgentData 删掉 before 之前的审计日志和用量，以及早就决定了的申请。
func (s *Store) PurgeOldAgentData(ctx context.Context, before time.Time) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM agent_audit WHERE at < $1`, before); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM agent_usage WHERE day < ($1 AT TIME ZONE 'UTC')::date`, before); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `DELETE FROM agent_requests WHERE status <> 'pending' AND created_at < $1`, before)
	return err
}

// AgentRequest 是 agent 申请的一个危险操作（永久删除等），要人在网页上批准。
type AgentRequest struct {
	ID        uuid.UUID   `json:"id"`
	TokenID   *uuid.UUID  `json:"token_id"`
	TokenName string      `json:"token_name"`
	Action    string      `json:"action"`
	FileIDs   []uuid.UUID `json:"file_ids"`
	Paths     []string    `json:"paths"`
	Reason    string      `json:"reason"`
	Status    string      `json:"status"`
	Result    string      `json:"result,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	DecidedAt *time.Time  `json:"decided_at,omitempty"`
}

const requestColumns = `id, token_id, token_name, action, file_ids, paths, reason, status, result, created_at, decided_at`

func scanRequest(row pgx.Row) (*AgentRequest, error) {
	var q AgentRequest
	err := row.Scan(&q.ID, &q.TokenID, &q.TokenName, &q.Action, &q.FileIDs, &q.Paths, &q.Reason, &q.Status, &q.Result, &q.CreatedAt, &q.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &q, err
}

// AgentRequestTTL：申请这么久没人批就作废。
const AgentRequestTTL = 24 * time.Hour

// CreateAgentRequest 存一个待批准的申请。
func (s *Store) CreateAgentRequest(ctx context.Context, userID uuid.UUID, q *AgentRequest) (*AgentRequest, error) {
	if q.FileIDs == nil {
		q.FileIDs = []uuid.UUID{}
	}
	if q.Paths == nil {
		q.Paths = []string{}
	}
	return scanRequest(s.db.QueryRow(ctx, `
		INSERT INTO agent_requests (user_id, token_id, token_name, action, file_ids, paths, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+requestColumns,
		userID, q.TokenID, q.TokenName, q.Action, q.FileIDs, q.Paths, q.Reason))
}

// expireAgentRequests 把放了超过 AgentRequestTTL 还没批的申请标成作废。
func (s *Store) expireAgentRequests(ctx context.Context, userID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `
		UPDATE agent_requests SET status = 'expired', decided_at = now()
		WHERE user_id = $1 AND status = 'pending' AND created_at < now() - $2::interval`, userID, AgentRequestTTL.String())
	return err
}

// GetAgentRequest 返回一个申请。
func (s *Store) GetAgentRequest(ctx context.Context, userID, id uuid.UUID) (*AgentRequest, error) {
	if err := s.expireAgentRequests(ctx, userID); err != nil {
		return nil, err
	}
	return scanRequest(s.db.QueryRow(ctx, `SELECT `+requestColumns+` FROM agent_requests WHERE id = $1 AND user_id = $2`, id, userID))
}

// ListAgentRequests 列出申请（status 为空时全部），最新的在前，最多 200 个。
func (s *Store) ListAgentRequests(ctx context.Context, userID uuid.UUID, status string) ([]*AgentRequest, error) {
	if err := s.expireAgentRequests(ctx, userID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT `+requestColumns+` FROM agent_requests WHERE user_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY created_at DESC LIMIT 200`, userID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentRequest{}
	for rows.Next() {
		q, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// DecideAgentRequest 把一个还在等的申请标成 status（approved / rejected / failed）。不是在等的返回 ErrConflict。
func (s *Store) DecideAgentRequest(ctx context.Context, userID, id uuid.UUID, status, result string) (*AgentRequest, error) {
	if err := s.expireAgentRequests(ctx, userID); err != nil {
		return nil, err
	}
	q, err := scanRequest(s.db.QueryRow(ctx, `
		UPDATE agent_requests SET status = $3, result = $4, decided_at = now()
		WHERE id = $1 AND user_id = $2 AND status = 'pending' RETURNING `+requestColumns, id, userID, status, result))
	if errors.Is(err, ErrNotFound) {
		if _, err := s.GetAgentRequest(ctx, userID, id); err != nil {
			return nil, err
		}
		return nil, ErrConflict
	}
	return q, err
}

// SetAgentRequestResult 批准后执行完，记下结果（执行失败时状态改成 failed）。
func (s *Store) SetAgentRequestResult(ctx context.Context, id uuid.UUID, status, result string) error {
	_, err := s.db.Exec(ctx, `UPDATE agent_requests SET status = $2, result = $3 WHERE id = $1`, id, status, result)
	return err
}

// Ancestry 返回一个条目和它所有的上级文件夹：自己在最前，根目录下那一层在最后。包括回收站里的。
func (s *Store) Ancestry(ctx context.Context, userID, id uuid.UUID) ([]*File, error) {
	rows, err := s.db.Query(ctx, `
		WITH RECURSIVE up AS (
			SELECT `+fileColumns+`, 0 AS depth FROM files WHERE id = $1 AND user_id = $2 AND status = 'active'
			UNION ALL
			SELECT `+prefixed("f", fileColumns)+`, up.depth + 1 FROM files f JOIN up ON f.id = up.parent_id WHERE up.depth < 64
		)
		SELECT `+fileColumns+` FROM up ORDER BY depth`, id, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// ChildByName 返回文件夹里（parent 为 nil 是根目录）叫 name 的条目（不在回收站里的）。
func (s *Store) ChildByName(ctx context.Context, userID uuid.UUID, parent *uuid.UUID, name string) (*File, error) {
	return scanFile(s.db.QueryRow(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE user_id = $1 AND parent_id IS NOT DISTINCT FROM $2 AND name = $3 AND status = 'active' AND trashed_at IS NULL`,
		userID, parent, name))
}

// TreeQuery 在一棵子树（Root 为 nil 是整个网盘）里找条目，跳过回收站里的；AllowPrivate 为 false 时私密文件夹连同里面的都跳过。
type TreeQuery struct {
	Root         *uuid.UUID
	AllowPrivate bool
	Name         string // 名字里包含（不分大小写）
	Type         string // file | folder
	MimePrefix   string // 如 "image/"
	From, To     *time.Time
	MinSize      *int64
	MaxSize      *int64
	IDs          []uuid.UUID // 只看这些
	Limit        int
	Offset       int
}

// TreeItem 是子树里的一个条目，Path 从子树的根算起，以 "/" 开头。
type TreeItem struct {
	*File
	Path string `json:"path"`
}

const treeCTE = `
	WITH RECURSIVE tree AS (
		SELECT id, '/' || name AS path, 1 AS depth FROM files
		WHERE user_id = $1 AND parent_id IS NOT DISTINCT FROM $2 AND status = 'active' AND trashed_at IS NULL AND ($3 OR NOT private)
		UNION ALL
		SELECT c.id, t.path || '/' || c.name, t.depth + 1 FROM files c JOIN tree t ON c.parent_id = t.id
		WHERE c.status = 'active' AND c.trashed_at IS NULL AND ($3 OR NOT c.private) AND t.depth < 64
	)`

// SearchTree 按条件在子树里找条目：文件夹在前，文件按拍摄时间（没有时按上传时间）新的在前。
func (s *Store) SearchTree(ctx context.Context, userID uuid.UUID, q TreeQuery) ([]TreeItem, error) {
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 100
	}
	like := ""
	if q.Name != "" {
		like = "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q.Name) + "%"
	}
	var ids []uuid.UUID
	if q.IDs != nil {
		ids = q.IDs
	}
	rows, err := s.db.Query(ctx, treeCTE+`
		SELECT `+prefixed("f", fileColumns)+`, t.path FROM tree t JOIN files f ON f.id = t.id
		WHERE ($4 = '' OR f.name ILIKE $4) AND ($5 = '' OR f.type = $5) AND ($6 = '' OR f.mime LIKE $6 || '%')
		  AND ($7::timestamptz IS NULL OR COALESCE(f.taken_at, f.created_at) >= $7)
		  AND ($8::timestamptz IS NULL OR COALESCE(f.taken_at, f.created_at) < $8)
		  AND ($9::bigint IS NULL OR (f.type = 'file' AND f.size >= $9))
		  AND ($10::bigint IS NULL OR (f.type = 'file' AND f.size <= $10))
		  AND ($11::uuid[] IS NULL OR f.id = ANY($11))
		ORDER BY f.type DESC, COALESCE(f.taken_at, f.created_at) DESC, t.path
		LIMIT $12 OFFSET $13`,
		userID, q.Root, q.AllowPrivate, like, q.Type, q.MimePrefix, q.From, q.To, q.MinSize, q.MaxSize, ids, q.Limit, q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TreeItem{}
	for rows.Next() {
		var f File
		var it TreeItem
		if err := rows.Scan(&f.ID, &f.UserID, &f.ParentID, &f.Name, &f.Type, &f.Size, &f.Mime,
			&f.SHA256, &f.DEKWrapped, &f.Status, &f.TrashedAt, &f.TakenAt, &f.Private, &f.CreatedAt, &f.UpdatedAt, &it.Path); err != nil {
			return nil, err
		}
		it.File = &f
		out = append(out, it)
	}
	return out, rows.Err()
}

// TreeStats 统计子树里的文件数量和大小（规则和 FolderStats 一样，另外按 AllowPrivate 跳过私密文件夹）。
func (s *Store) TreeStats(ctx context.Context, userID uuid.UUID, root *uuid.UUID, allowPrivate bool) (DriveStats, error) {
	var st DriveStats
	err := s.db.QueryRow(ctx, treeCTE+`, kinds AS (
			SELECT id, type, size, type = 'file' AND `+isPhotoSQL+` AS photo,
			       type = 'file' AND NOT `+isPhotoSQL+` AND `+isVideoSQL+` AS video
			FROM (SELECT f.id, f.type, f.size, f.mime, f.name FROM tree t JOIN files f ON f.id = t.id) x
		)
		SELECT count(*) FILTER (WHERE type = 'folder'), count(*) FILTER (WHERE type = 'file'),
		       count(*) FILTER (WHERE photo), count(*) FILTER (WHERE video),
		       COALESCE(sum(size) FILTER (WHERE type = 'file'), 0),
		       COALESCE(sum(size) FILTER (WHERE photo), 0), COALESCE(sum(size) FILTER (WHERE video), 0),
		       count(*) FILTER (WHERE type = 'file' AND NOT EXISTS (
		           SELECT 1 FROM file_parts p WHERE p.file_id = kinds.id AND p.backup_message_id IS NULL))
		FROM kinds`, userID, root, allowPrivate).
		Scan(&st.Folders, &st.Files, &st.Photos, &st.Videos, &st.Bytes, &st.PhotoBytes, &st.VideoBytes, &st.BackedUpFiles)
	st.Others = st.Files - st.Photos - st.Videos
	return st, err
}

// CountFilesUnder 数一个文件夹里（任意层，不在回收站里的）有几个文件；是文件时返回 1。
func (s *Store) CountFilesUnder(ctx context.Context, userID uuid.UUID, f *File) (int64, error) {
	if f.Type == TypeFile {
		return 1, nil
	}
	var n int64
	err := s.db.QueryRow(ctx, `
		WITH RECURSIVE tree AS (
			SELECT id, type FROM files WHERE user_id = $1 AND parent_id = $2 AND status = 'active' AND trashed_at IS NULL
			UNION ALL
			SELECT c.id, c.type FROM files c JOIN tree t ON c.parent_id = t.id WHERE c.status = 'active' AND c.trashed_at IS NULL
		)
		SELECT count(*) FROM tree WHERE type = 'file'`, userID, f.ID).Scan(&n)
	return n, err
}

// ListAgentShares 列出一把 agent 令牌建的分享链接。
func (s *Store) ListAgentShares(ctx context.Context, userID, tokenID uuid.UUID) ([]*Share, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+shareColumns+`, `+prefixed("f", fileColumns)+`
		FROM shares s JOIN files f ON f.id = s.file_id
		WHERE s.user_id = $1 AND s.agent_token_id = $2 ORDER BY s.created_at DESC`, userID, tokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Share{}
	for rows.Next() {
		sh, err := scanShareRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// DeleteAgentShare 撤销一把 agent 令牌自己建的分享链接。
func (s *Store) DeleteAgentShare(ctx context.Context, userID, tokenID, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM shares WHERE id = $1 AND user_id = $2 AND agent_token_id = $3`, id, userID, tokenID)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	return err
}

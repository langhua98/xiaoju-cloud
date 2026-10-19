package meta

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrNotFolder：只有文件夹能设成私密。
	ErrNotFolder = errors.New("only folders can be private")
	// ErrMoveIntoSelf：文件夹不能移进它自己或它的子文件夹里。
	ErrMoveIntoSelf = errors.New("cannot move a folder into itself")
)

// FileUpdate 是要改的地方，nil 表示不改。Move 为 true 时把条目移到 ParentID（nil 是根目录）。
type FileUpdate struct {
	Name     *string
	Move     bool
	ParentID *uuid.UUID
	Private  *bool
}

// UpdateFile 重命名、移动一个条目，或者把文件夹设成 / 取消私密。目标文件夹里重名时返回 ErrConflict。
func (s *Store) UpdateFile(ctx context.Context, userID, id uuid.UUID, u FileUpdate) (*File, error) {
	f, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if f.TrashedAt != nil {
		return nil, ErrNotFound
	}
	if u.Private != nil && f.Type != TypeFolder {
		return nil, ErrNotFolder
	}
	parent := f.ParentID
	if u.Move {
		if err := s.checkParent(ctx, userID, u.ParentID); err != nil {
			return nil, err
		}
		if f.Type == TypeFolder && u.ParentID != nil {
			inside, err := s.isInside(ctx, *u.ParentID, f.ID)
			if err != nil {
				return nil, err
			}
			if inside {
				return nil, ErrMoveIntoSelf
			}
		}
		parent = u.ParentID
	}
	out, err := scanFile(s.db.QueryRow(ctx, `
		UPDATE files SET name = COALESCE($3, name), parent_id = $4, private = COALESCE($5, private), updated_at = now()
		WHERE id = $1 AND user_id = $2 AND status = 'active' AND trashed_at IS NULL
		RETURNING `+fileColumns, id, userID, u.Name, parent, u.Private))
	return out, mapErr(err)
}

// isInside：folder 是 ancestor 本身，或者在 ancestor 里面（任意层）。
func (s *Store) isInside(ctx context.Context, folder, ancestor uuid.UUID) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `
		WITH RECURSIVE up AS (
			SELECT id, parent_id, 0 AS depth FROM files WHERE id = $1
			UNION ALL
			SELECT f.id, f.parent_id, up.depth + 1 FROM files f JOIN up ON f.id = up.parent_id WHERE up.depth < 64
		)
		SELECT EXISTS (SELECT 1 FROM up WHERE id = $2)`, folder, ancestor).Scan(&ok)
	return ok, err
}

// Restore 把回收站里的条目放回原处（原来的文件夹也不在了时放回根目录）。原处已有同名的返回 ErrConflict。
func (s *Store) Restore(ctx context.Context, userID, id uuid.UUID) (*File, error) {
	f, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if f.TrashedAt == nil {
		return f, nil
	}
	parent := f.ParentID
	if err := s.checkParent(ctx, userID, parent); errors.Is(err, ErrNotFound) {
		parent = nil
	} else if err != nil {
		return nil, err
	}
	out, err := scanFile(s.db.QueryRow(ctx, `
		UPDATE files SET trashed_at = NULL, parent_id = $3, updated_at = now()
		WHERE id = $1 AND user_id = $2 AND status = 'active'
		RETURNING `+fileColumns, id, userID, parent))
	return out, mapErr(err)
}

// ListTrash 列出回收站：自己被移入回收站的条目（跟着文件夹一起进回收站的不单独列），最近删的在前。
// 先单独删了、后来所在的文件夹也删了的照样列出来，恢复时放回根目录。
func (s *Store) ListTrash(ctx context.Context, userID uuid.UUID) ([]*File, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE user_id = $1 AND status = 'active' AND trashed_at IS NOT NULL
		ORDER BY trashed_at DESC, name`, userID)
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
	return out, rows.Err()
}

// FilesUnder 列出文件夹里（任意层，包括回收站里的）所有文件的 ID，彻底删除文件夹时一个个删。
func (s *Store) FilesUnder(ctx context.Context, userID, folderID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx, `
		WITH RECURSIVE tree AS (
			SELECT id, type FROM files WHERE user_id = $1 AND parent_id = $2
			UNION ALL
			SELECT c.id, c.type FROM files c JOIN tree t ON c.parent_id = t.id
		)
		SELECT id FROM tree WHERE type = 'file'`, userID, folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// IngestCheck 是快捷指令最近一次问“哪些还没传过”时的清单（见迁移 0010）。UploadedSince：那之后这个相册里新传了几个。
type IngestCheck struct {
	Folder        string    `json:"folder"`
	ByDate        bool      `json:"by_date"`
	CheckedAt     time.Time `json:"checked_at"`
	Files         int       `json:"files"`
	Uploaded      int       `json:"uploaded"`
	Todo          int       `json:"todo"`
	UploadedSince int       `json:"uploaded_since"`
}

// RecordIngestCheck 记下一次清单（同一个相册、同一种方式只留最近一次）。
func (s *Store) RecordIngestCheck(ctx context.Context, userID uuid.UUID, c IngestCheck) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO ingest_checks (user_id, folder, by_date, checked_at, files, uploaded, todo) VALUES ($1, $2, $3, now(), $4, $5, $6)
		ON CONFLICT (user_id, folder, by_date) DO UPDATE
		SET checked_at = now(), files = EXCLUDED.files, uploaded = EXCLUDED.uploaded, todo = EXCLUDED.todo`,
		userID, c.Folder, c.ByDate, c.Files, c.Uploaded, c.Todo)
	return err
}

// AlbumBackup 是根目录下一个相册文件夹的备份情况。
type AlbumBackup struct {
	Folder       string        `json:"folder"`
	ID           *uuid.UUID    `json:"id,omitempty"` // 还没有这个文件夹时为 nil
	Files        int           `json:"files"`
	LastUploadAt *time.Time    `json:"last_upload_at,omitempty"`
	Checks       []IngestCheck `json:"checks"`
}

// PhoneBackup 返回几个相册文件夹（根目录下，按名字）的文件数、最近一次上传时间和快捷指令最近的清单。
func (s *Store) PhoneBackup(ctx context.Context, userID uuid.UUID, folders []string) ([]AlbumBackup, error) {
	rows, err := s.db.Query(ctx, `
		SELECT a.name, f.id,
		       (SELECT count(*) FROM files x WHERE x.parent_id = f.id AND x.type = 'file' AND x.status = 'active' AND x.trashed_at IS NULL),
		       (SELECT max(x.created_at) FROM files x WHERE x.parent_id = f.id AND x.type = 'file' AND x.status = 'active' AND x.trashed_at IS NULL)
		FROM unnest($2::text[]) WITH ORDINALITY AS a(name, n)
		LEFT JOIN files f ON f.user_id = $1 AND f.parent_id IS NULL AND f.name = a.name
		     AND f.type = 'folder' AND f.status = 'active' AND f.trashed_at IS NULL
		ORDER BY a.n`, userID, folders)
	if err != nil {
		return nil, err
	}
	var out []AlbumBackup
	index := map[string]int{}
	for rows.Next() {
		var a AlbumBackup
		if err := rows.Scan(&a.Folder, &a.ID, &a.Files, &a.LastUploadAt); err != nil {
			rows.Close()
			return nil, err
		}
		a.Checks = []IngestCheck{}
		index[a.Folder] = len(out)
		out = append(out, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.Query(ctx, `
		SELECT c.folder, c.by_date, c.checked_at, c.files, c.uploaded, c.todo,
		       (SELECT count(*) FROM files x JOIN files p ON x.parent_id = p.id
		        WHERE p.user_id = $1 AND p.parent_id IS NULL AND p.name = c.folder AND p.type = 'folder' AND p.trashed_at IS NULL
		          AND x.type = 'file' AND x.status = 'active' AND x.trashed_at IS NULL AND x.created_at > c.checked_at)
		FROM ingest_checks c WHERE c.user_id = $1
		ORDER BY c.checked_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c IngestCheck
		if err := rows.Scan(&c.Folder, &c.ByDate, &c.CheckedAt, &c.Files, &c.Uploaded, &c.Todo, &c.UploadedSince); err != nil {
			return nil, err
		}
		if i, ok := index[c.Folder]; ok {
			out[i].Checks = append(out[i].Checks, c)
		}
	}
	return out, rows.Err()
}

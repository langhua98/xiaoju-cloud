package storage

import (
	"context"
	"errors"
	"io"
)

// ErrReadOnly 是只读备用模式下想往存储端写或删时返回的错。
var ErrReadOnly = errors.New("storage: read-only standby, cannot write or delete")

// ReadOnly 包一层只能读的驱动（只读备用模式用）：Put、Delete 一律失败。
// 故意不转发 Pinner、Lister，备用服务不会置顶备份、也不会对账清理残块。
func ReadOnly(d Driver) Driver { return readOnly{d} }

type readOnly struct{ d Driver }

func (r readOnly) Name() string { return r.d.Name() }

func (r readOnly) Put(context.Context, PutRequest) (Location, error) {
	return Location{}, ErrReadOnly
}

func (r readOnly) ReadRange(ctx context.Context, loc Location, offset, length int64) (io.ReadCloser, error) {
	return r.d.ReadRange(ctx, loc, offset, length)
}

func (r readOnly) Delete(context.Context, []Location) error { return ErrReadOnly }

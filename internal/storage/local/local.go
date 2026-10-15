// Package local 是写本地磁盘的存储驱动，用于开发和测试（不需要 Telegram 账号）。
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

type Driver struct {
	dir  string
	next atomic.Int64
}

func New(dir string) (*Driver, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	d := &Driver{dir: dir}
	d.next.Store(time.Now().UnixNano())
	return d, nil
}

func (d *Driver) Name() string { return "local" }

func (d *Driver) path(loc storage.Location) string {
	return filepath.Join(d.dir, strconv.FormatInt(loc.MessageID, 10))
}

// 分块文件名另存一个旁路文件，对账时用来识别本程序上传的分块
const nameSuffix = ".name"

func (d *Driver) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	loc := storage.Location{MessageID: d.next.Add(1)}
	f, err := os.OpenFile(d.path(loc), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return storage.Location{}, err
	}
	n, err := io.Copy(f, req.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != req.Size {
		err = fmt.Errorf("local: wrote %d bytes, expected %d", n, req.Size)
	}
	if err == nil && req.Name != "" {
		err = os.WriteFile(d.path(loc)+nameSuffix, []byte(req.Name), 0o640)
	}
	if err != nil {
		os.Remove(d.path(loc))
		os.Remove(d.path(loc) + nameSuffix)
		return storage.Location{}, err
	}
	return loc, nil
}

func (d *Driver) ReadRange(ctx context.Context, loc storage.Location, offset, length int64) (io.ReadCloser, error) {
	f, err := os.Open(d.path(loc))
	if errors.Is(err, os.ErrNotExist) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(f, offset, length), f}, nil
}

func (d *Driver) Delete(ctx context.Context, locs []storage.Location) error {
	var errs []error
	for _, loc := range locs {
		if err := os.Remove(d.path(loc)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		os.Remove(d.path(loc) + nameSuffix)
	}
	return errors.Join(errs...)
}

func (d *Driver) List(ctx context.Context, _ int64, fn func(storage.Object) error) error {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil || e.IsDir() {
			continue // 旁路文件等
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		obj := storage.Object{Location: storage.Location{MessageID: id}, Size: info.Size(), Created: info.ModTime()}
		if name, err := os.ReadFile(d.path(obj.Location) + nameSuffix); err == nil {
			obj.Name = string(name)
		}
		if err := fn(obj); err != nil {
			return err
		}
	}
	return nil
}

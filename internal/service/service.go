// Package service 实现云盘核心业务：分块加密上传、解密下载、删除。
package service

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

type Service struct {
	store     *meta.Store
	driver    storage.Driver
	masterKey []byte
	partSize  int64
	tempDir   string
	log       *slog.Logger
}

type Options struct {
	MasterKey []byte
	PartSize  int64
	TempDir   string
	Log       *slog.Logger
}

func New(store *meta.Store, driver storage.Driver, opt Options) *Service {
	return &Service{
		store:     store,
		driver:    driver,
		masterKey: opt.MasterKey,
		partSize:  opt.PartSize,
		tempDir:   opt.TempDir,
		log:       opt.Log,
	}
}

func (s *Service) Store() *meta.Store { return s.store }

type UploadRequest struct {
	UserID   uuid.UUID
	ParentID *uuid.UUID
	Name     string
	Mime     string
	Body     io.Reader
}

// Upload 把 Body 按 partSize 切块：每块先加密写入临时文件（便于失败重试、长度已知），
// 再交给存储驱动上传，最后记录分块映射并激活文件。失败时清理已上传的分块。
func (s *Service) Upload(ctx context.Context, req UploadRequest) (_ *meta.File, err error) {
	dek, err := crypto.NewKey()
	if err != nil {
		return nil, err
	}
	wrapped, err := crypto.WrapKey(s.masterKey, dek)
	if err != nil {
		return nil, err
	}
	f, err := s.store.CreateUpload(ctx, &meta.File{
		UserID: req.UserID, ParentID: req.ParentID, Name: req.Name, Mime: req.Mime, DEKWrapped: wrapped,
	})
	if err != nil {
		return nil, err
	}

	var uploaded []storage.Location
	defer func() {
		if err == nil {
			return
		}
		// 用独立 context 清理，避免请求被取消后无法清理
		cleanup := context.WithoutCancel(ctx)
		if len(uploaded) > 0 {
			if derr := s.driver.Delete(cleanup, uploaded); derr != nil {
				s.log.Error("cleanup uploaded parts", "file", f.ID, "err", derr)
			}
		}
		if derr := s.store.DeleteRow(cleanup, f.ID); derr != nil {
			s.log.Error("cleanup file row", "file", f.ID, "err", derr)
		}
	}()

	fileHash := sha256.New()
	body := io.TeeReader(req.Body, fileHash)
	var total int64
	for partNo := 0; ; partNo++ {
		part, loc, perr := s.uploadPart(ctx, f.ID, partNo, dek, body)
		if perr != nil {
			return nil, perr
		}
		if part == nil {
			break // 输入已读完
		}
		uploaded = append(uploaded, loc)
		if err := s.store.AddPart(ctx, part); err != nil {
			return nil, err
		}
		total += part.Size
		if part.Size < s.partSize {
			break
		}
	}
	return s.store.ActivateFile(ctx, f.ID, total, fileHash.Sum(nil))
}

// uploadPart 读取最多 partSize 字节、加密后上传。输入为空时返回 (nil, _, nil)。
func (s *Service) uploadPart(ctx context.Context, fileID uuid.UUID, partNo int, dek []byte, body io.Reader) (*meta.Part, storage.Location, error) {
	tmp, err := os.CreateTemp(s.tempDir, "xiaoju-part-*")
	if err != nil {
		return nil, storage.Location{}, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	iv, err := crypto.NewIV()
	if err != nil {
		return nil, storage.Location{}, err
	}
	stream, err := crypto.NewCTRAt(dek, iv, 0)
	if err != nil {
		return nil, storage.Location{}, err
	}
	partHash := sha256.New()
	enc := &cipher.StreamWriter{S: stream, W: io.MultiWriter(tmp, partHash)}
	n, err := io.Copy(enc, io.LimitReader(body, s.partSize))
	if err != nil {
		return nil, storage.Location{}, fmt.Errorf("read upload body: %w", err)
	}
	if n == 0 {
		return nil, storage.Location{}, nil // 空文件没有分块
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, storage.Location{}, err
	}

	sum := partHash.Sum(nil)
	loc, err := s.driver.Put(ctx, storage.PutRequest{
		Name:    fmt.Sprintf("%s.part%04d", fileID, partNo),
		Caption: caption(fileID, partNo, sum),
		Body:    tmp,
		Size:    n,
	})
	if err != nil {
		return nil, storage.Location{}, err
	}
	return &meta.Part{
		FileID: fileID, PartNo: partNo, Storage: s.driver.Name(),
		ChannelID: loc.ChannelID, MessageID: loc.MessageID,
		Size: n, IV: iv, SHA256: sum,
	}, loc, nil
}

// caption 写入消息说明，数据库丢失时可以扫描频道重建 文件ID→分块 的映射（文件名等仍需数据库）。
func caption(fileID uuid.UUID, partNo int, sum []byte) string {
	b, _ := json.Marshal(map[string]any{"f": fileID, "p": partNo, "sha256": hex.EncodeToString(sum)})
	return string(b)
}

// Open 返回文件的明文读取器。
func (s *Service) Open(ctx context.Context, userID, id uuid.UUID) (*meta.File, *FileReader, error) {
	f, err := s.store.Get(ctx, userID, id)
	if err != nil {
		return nil, nil, err
	}
	if f.Type != meta.TypeFile || f.TrashedAt != nil {
		return nil, nil, meta.ErrNotFound
	}
	dek, err := crypto.UnwrapKey(s.masterKey, f.DEKWrapped)
	if err != nil {
		return nil, nil, fmt.Errorf("unwrap file key: %w", err)
	}
	parts, err := s.store.ListParts(ctx, f.ID)
	if err != nil {
		return nil, nil, err
	}
	return f, NewFileReader(ctx, s.driver, dek, parts), nil
}

var ErrFolderPurge = errors.New("permanent delete of folders is not supported yet; move to trash instead")

// Purge 永久删除文件：先删存储端分块，再删数据库记录。
func (s *Service) Purge(ctx context.Context, userID, id uuid.UUID) error {
	f, err := s.store.Get(ctx, userID, id)
	if err != nil {
		return err
	}
	if f.Type != meta.TypeFile {
		return ErrFolderPurge
	}
	parts, err := s.store.ListParts(ctx, f.ID)
	if err != nil {
		return err
	}
	locs := make([]storage.Location, 0, len(parts))
	for _, p := range parts {
		locs = append(locs, storage.Location{ChannelID: p.ChannelID, MessageID: p.MessageID})
	}
	if err := s.driver.Delete(ctx, locs); err != nil {
		return err
	}
	return s.store.DeleteRow(ctx, f.ID)
}

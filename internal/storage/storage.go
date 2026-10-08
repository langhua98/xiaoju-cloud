// Package storage 定义存储驱动接口。驱动只负责存取"不透明的加密分块"，
// 目录、文件名、分块映射等元数据全部由数据库管理。
package storage

import (
	"context"
	"errors"
	"io"
)

// Location 指向一个已存储的分块。对 Telegram 来说就是 频道ID + 消息ID。
type Location struct {
	ChannelID int64
	MessageID int64
}

type PutRequest struct {
	Name    string    // 分块在存储端的文件名，如 <file-uuid>.part0001
	Caption string    // 附加信息（Telegram 写入消息说明，用于灾难时重建元数据）
	Body    io.Reader // 加密后的分块内容
	Size    int64
}

type Driver interface {
	// Name 驱动名，写入 file_parts.storage。
	Name() string
	Put(ctx context.Context, req PutRequest) (Location, error)
	// ReadRange 读取分块 [offset, offset+length) 范围的字节。
	ReadRange(ctx context.Context, loc Location, offset, length int64) (io.ReadCloser, error)
	Delete(ctx context.Context, locs []Location) error
}

var ErrNotFound = errors.New("storage: part not found")

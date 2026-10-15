// Package storage 定义存储驱动接口。驱动只负责存取"不透明的加密分块"，
// 目录、文件名、分块映射等元数据全部由数据库管理。
package storage

import (
	"context"
	"errors"
	"io"
	"time"
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

// Object 是存储端实际存在的一个分块，用于对账。
type Object struct {
	Location
	Name    string // 上传时的文件名（PutRequest.Name）；不是本程序上传的为空
	Size    int64
	Created time.Time
}

// Lister 是可以列出全部已存储分块的驱动（对账、清理残块用）。
type Lister interface {
	// List 逐个回调存储端的分块。minMessageID 是已知存在的最大消息 ID，
	// 实现至少要扫描到这里，避免因为中间有大段被删除的消息而提前停止。
	// 扫描出错时返回错误，调用方必须把结果当作不完整。
	List(ctx context.Context, minMessageID int64, fn func(Object) error) error
}

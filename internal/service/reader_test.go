package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
)

// 把 plain 按 partSize 加密切块写进本地驱动，返回分块记录。
func writeParts(t *testing.T, d storage.Driver, dek, plain []byte, partSize int) []meta.Part {
	t.Helper()
	var parts []meta.Part
	for i := 0; i*partSize < len(plain); i++ {
		chunk := plain[i*partSize : min((i+1)*partSize, len(plain))]
		iv, _ := crypto.NewIV()
		s, _ := crypto.NewCTRAt(dek, iv, 0)
		ct := make([]byte, len(chunk))
		s.XORKeyStream(ct, chunk)
		loc, err := d.Put(context.Background(), storage.PutRequest{Name: "p", Body: bytes.NewReader(ct), Size: int64(len(ct))})
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, meta.Part{PartNo: i, ChannelID: loc.ChannelID, MessageID: loc.MessageID, Size: int64(len(ct)), IV: iv})
	}
	return parts
}

func TestFileReaderSeekAcrossParts(t *testing.T) {
	d, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dek, _ := crypto.NewKey()
	plain := make([]byte, 10_000)
	rand.Read(plain)
	parts := writeParts(t, d, dek, plain, 3000) // 4 块：3000,3000,3000,1000

	r := NewFileReader(context.Background(), d, dek, parts)
	defer r.Close()
	if r.Size() != int64(len(plain)) {
		t.Fatalf("size = %d", r.Size())
	}

	all, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(all, plain) {
		t.Fatalf("full read mismatch: err=%v len=%d", err, len(all))
	}

	for _, rg := range []struct{ off, n int64 }{
		{0, 1}, {2999, 2}, {2995, 6010}, {5000, 5000}, {9999, 1}, {17, 3000},
	} {
		if _, err := r.Seek(rg.off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, rg.n)
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatalf("range %+v: %v", rg, err)
		}
		if !bytes.Equal(got, plain[rg.off:rg.off+rg.n]) {
			t.Fatalf("range %+v: content mismatch", rg)
		}
	}

	if end, _ := r.Seek(0, io.SeekEnd); end != int64(len(plain)) {
		t.Fatalf("seek end = %d", end)
	}
	if n, err := r.Read(make([]byte, 10)); n != 0 || err != io.EOF {
		t.Fatalf("read at end = %d, %v", n, err)
	}
}

func TestFileReaderEmpty(t *testing.T) {
	r := NewFileReader(context.Background(), nil, nil, nil)
	b, err := io.ReadAll(r)
	if err != nil || len(b) != 0 {
		t.Fatalf("empty read: %v %d", err, len(b))
	}
}

// mirrorParts 把主驱动里的分块密文复制到备份驱动，并在分块记录里填上备份位置。
func mirrorParts(t *testing.T, primary, backup storage.Driver, parts []meta.Part) {
	t.Helper()
	for i := range parts {
		p := &parts[i]
		rc, err := primary.ReadRange(context.Background(), storage.Location{ChannelID: p.ChannelID, MessageID: p.MessageID}, 0, p.Size)
		if err != nil {
			t.Fatal(err)
		}
		loc, err := backup.Put(context.Background(), storage.PutRequest{Name: "b", Body: rc, Size: p.Size})
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		p.BackupChannelID, p.BackupMessageID = &loc.ChannelID, &loc.MessageID
	}
}

// flakyDriver 模拟主存储故障：broken 中的分块要么无法打开，要么读到 failAfter 字节后出错。
type flakyDriver struct {
	storage.Driver
	broken    map[int64]bool
	failAfter int64 // <0 表示直接打不开
}

var errFlaky = errors.New("simulated storage failure")

func (d *flakyDriver) ReadRange(ctx context.Context, loc storage.Location, off, n int64) (io.ReadCloser, error) {
	if !d.broken[loc.MessageID] {
		return d.Driver.ReadRange(ctx, loc, off, n)
	}
	if d.failAfter < 0 {
		return nil, errFlaky
	}
	rc, err := d.Driver.ReadRange(ctx, loc, off, n)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(rc, d.failAfter), errReader{}), rc}, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errFlaky }

func TestFileReaderFallsBackToBackup(t *testing.T) {
	primary, _ := local.New(t.TempDir())
	backup, _ := local.New(t.TempDir())
	dek, _ := crypto.NewKey()
	plain := make([]byte, 10_000)
	rand.Read(plain)
	parts := writeParts(t, primary, dek, plain, 3000)
	mirrorParts(t, primary, backup, parts)

	for _, failAfter := range []int64{-1, 0, 100} {
		// 第 2、4 块在主存储里坏了
		flaky := &flakyDriver{Driver: primary, failAfter: failAfter, broken: map[int64]bool{
			parts[1].MessageID: true, parts[3].MessageID: true,
		}}
		r := NewFileReader(context.Background(), flaky, dek, parts)
		r.backup = backup
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("failAfter=%d: err=%v equal=%v", failAfter, err, bytes.Equal(got, plain))
		}

		// Range 读取跨越坏块
		r = NewFileReader(context.Background(), flaky, dek, parts)
		r.backup = backup
		r.Seek(2990, io.SeekStart)
		buf := make([]byte, 3020)
		if _, err := io.ReadFull(r, buf); err != nil || !bytes.Equal(buf, plain[2990:6010]) {
			t.Fatalf("failAfter=%d range: err=%v", failAfter, err)
		}
	}

	// 没有备份时错误照常返回
	flaky := &flakyDriver{Driver: primary, failAfter: -1, broken: map[int64]bool{parts[0].MessageID: true}}
	noBackup := append([]meta.Part(nil), parts...)
	noBackup[0].BackupChannelID, noBackup[0].BackupMessageID = nil, nil
	r := NewFileReader(context.Background(), flaky, dek, noBackup)
	r.backup = backup
	if _, err := io.ReadAll(r); !errors.Is(err, errFlaky) {
		t.Fatalf("want simulated failure without backup, got %v", err)
	}
}

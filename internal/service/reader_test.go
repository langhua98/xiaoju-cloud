package service

import (
	"bytes"
	"context"
	"crypto/rand"
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

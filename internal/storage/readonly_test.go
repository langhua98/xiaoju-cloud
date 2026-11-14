package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeDriver struct{ puts, deletes int }

func (f *fakeDriver) Name() string { return "fake" }
func (f *fakeDriver) Put(context.Context, PutRequest) (Location, error) {
	f.puts++
	return Location{MessageID: 1}, nil
}
func (f *fakeDriver) ReadRange(context.Context, Location, int64, int64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("data")), nil
}
func (f *fakeDriver) Delete(context.Context, []Location) error       { f.deletes++; return nil }
func (f *fakeDriver) Pin(context.Context, Location, bool) error      { return nil }
func (f *fakeDriver) Pinned(context.Context) (Object, string, error) { return Object{}, "", nil }

func TestReadOnly(t *testing.T) {
	f := &fakeDriver{}
	d := ReadOnly(f)
	if _, err := d.Put(context.Background(), PutRequest{}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Put err = %v", err)
	}
	if err := d.Delete(context.Background(), []Location{{MessageID: 1}}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Delete err = %v", err)
	}
	if f.puts != 0 || f.deletes != 0 {
		t.Fatalf("underlying driver was written: puts=%d deletes=%d", f.puts, f.deletes)
	}
	rc, err := d.ReadRange(context.Background(), Location{}, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "data" {
		t.Fatalf("ReadRange = %q", b)
	}
	if _, ok := d.(Pinner); ok {
		t.Fatal("read-only driver must not expose Pinner")
	}
	if d.Name() != "fake" {
		t.Fatal("Name not forwarded")
	}
}

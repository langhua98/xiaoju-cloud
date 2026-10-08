package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestCTRAtMatchesFullStream(t *testing.T) {
	key, _ := NewKey()
	plain := make([]byte, 5000)
	rand.Read(plain)

	ivs := [][]byte{
		mustIV(t),
		bytes.Repeat([]byte{0xff}, IVSize), // 测试进位溢出
		append(bytes.Repeat([]byte{0}, IVSize-1), 0xfe),
	}
	for _, iv := range ivs {
		full, _ := NewCTRAt(key, iv, 0)
		ct := make([]byte, len(plain))
		full.XORKeyStream(ct, plain)

		for _, off := range []int64{0, 1, 15, 16, 17, 255, 256, 4095, 4999} {
			s, err := NewCTRAt(key, iv, off)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(plain)-int(off))
			s.XORKeyStream(got, ct[off:])
			if !bytes.Equal(got, plain[off:]) {
				t.Fatalf("iv=%x offset=%d: decrypted mismatch", iv, off)
			}
		}
	}
}

func TestWrapUnwrap(t *testing.T) {
	master, _ := NewKey()
	dek, _ := NewKey()
	w, err := WrapKey(master, dek)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapKey(master, w)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("unwrap mismatch")
	}
	other, _ := NewKey()
	if _, err := UnwrapKey(other, w); err == nil {
		t.Fatal("expected error with wrong master key")
	}
}

func mustIV(t *testing.T) []byte {
	iv, err := NewIV()
	if err != nil {
		t.Fatal(err)
	}
	return iv
}

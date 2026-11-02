package dbdump

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 备份文件用主密钥派生出来的密钥加密（AES-256-GCM，按 1MB 分段，每段带认证；最后一段带“结束”标记，
// 文件被截断能发现）。文件里没有主密钥本身：没有主密钥打不开，主密钥要另外保管。
//
//	"XJDBBAK1" | 7 字节随机前缀 | 段……
//	段：1 字节标记（0 后面还有，1 最后一段）| 4 字节密文长度 | 密文
//	每段的 nonce = 前缀 | 4 字节段号 | 标记
const (
	sealMagic   = "XJDBBAK1"
	prefixSize  = 7
	segmentSize = 1 << 20
)

// BackupKey 从主密钥派生备份文件的密钥（和包裹文件密钥用的不是同一个）。
func BackupKey(master []byte) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte("xiaoju-cloud database backup v1"))
	return m.Sum(nil)
}

// sealWriter 把写进来的明文分段加密写到 w；Close 写出最后一段。
type sealWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	prefix []byte
	seq    uint32
	buf    []byte
	closed bool
}

// NewSealWriter 返回一个加密写入器；写完必须 Close（不会关闭 w）。
func NewSealWriter(w io.Writer, key []byte) (io.WriteCloser, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, prefixSize)
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, sealMagic); err != nil {
		return nil, err
	}
	if _, err := w.Write(prefix); err != nil {
		return nil, err
	}
	return &sealWriter{w: w, aead: aead, prefix: prefix, buf: make([]byte, 0, segmentSize)}, nil
}

func (s *sealWriter) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("seal: write after close")
	}
	n := 0
	for len(p) > 0 {
		if len(s.buf) == segmentSize {
			if err := s.flush(false); err != nil {
				return n, err
			}
		}
		k := copy(s.buf[len(s.buf):segmentSize], p)
		s.buf = s.buf[:len(s.buf)+k]
		p = p[k:]
		n += k
	}
	return n, nil
}

func (s *sealWriter) flush(last bool) error {
	flag := byte(0)
	if last {
		flag = 1
	}
	ct := s.aead.Seal(nil, s.nonce(flag), s.buf, nil)
	var hdr [5]byte
	hdr[0] = flag
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(ct)))
	if _, err := s.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := s.w.Write(ct); err != nil {
		return err
	}
	s.seq++
	s.buf = s.buf[:0]
	return nil
}

func (s *sealWriter) nonce(flag byte) []byte {
	n := make([]byte, 0, 12)
	n = append(n, s.prefix...)
	n = binary.BigEndian.AppendUint32(n, s.seq)
	return append(n, flag)
}

func (s *sealWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.flush(true)
}

// ErrWrongKey 表示备份文件解不开：主密钥不对，或者文件坏了。
var ErrWrongKey = errors.New("cannot decrypt the backup: wrong master key, or the file is damaged")

type openReader struct {
	r      *bufio.Reader
	aead   cipher.AEAD
	prefix []byte
	seq    uint32
	buf    []byte
	done   bool
}

// NewOpenReader 解密 NewSealWriter 写出的数据。读到最后一段之前文件就结束了会报错。
func NewOpenReader(r io.Reader, key []byte) (io.Reader, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(r, segmentSize+64)
	head := make([]byte, len(sealMagic)+prefixSize)
	if _, err := io.ReadFull(br, head); err != nil || string(head[:len(sealMagic)]) != sealMagic {
		return nil, errors.New("not a xiaoju database backup file")
	}
	return &openReader{r: br, aead: aead, prefix: head[len(sealMagic):]}, nil
}

func (o *openReader) Read(p []byte) (int, error) {
	for len(o.buf) == 0 {
		if o.done {
			return 0, io.EOF
		}
		var hdr [5]byte
		if _, err := io.ReadFull(o.r, hdr[:]); err != nil {
			return 0, errors.New("backup file is truncated")
		}
		flag, size := hdr[0], binary.BigEndian.Uint32(hdr[1:])
		if flag > 1 || size > segmentSize+uint32(o.aead.Overhead()) {
			return 0, ErrWrongKey
		}
		ct := make([]byte, size)
		if _, err := io.ReadFull(o.r, ct); err != nil {
			return 0, errors.New("backup file is truncated")
		}
		n := make([]byte, 0, 12)
		n = append(n, o.prefix...)
		n = binary.BigEndian.AppendUint32(n, o.seq)
		n = append(n, flag)
		pt, err := o.aead.Open(ct[:0], n, ct, nil)
		if err != nil {
			return 0, ErrWrongKey
		}
		o.seq++
		o.buf = pt
		if flag == 1 {
			o.done = true
			if _, err := o.r.Peek(1); err != io.EOF {
				return 0, fmt.Errorf("backup file has extra data after the end")
			}
		}
	}
	n := copy(p, o.buf)
	o.buf = o.buf[n:]
	return n, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

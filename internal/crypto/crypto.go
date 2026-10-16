// Package crypto 实现分块加密：每个文件一把随机 DEK（由主密钥用 AES-GCM 包裹），
// 每个分块用 AES-256-CTR 加密。CTR 可以从任意偏移解密，从而支持 HTTP Range。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

const (
	KeySize = 32
	IVSize  = aes.BlockSize
)

// NewKey 生成随机 32 字节密钥。
func NewKey() ([]byte, error) { return randBytes(KeySize) }

// NewIV 生成随机 16 字节初始计数器。
func NewIV() ([]byte, error) { return randBytes(IVSize) }

// WrapKey 用主密钥加密 DEK，输出 nonce || ciphertext。
func WrapKey(master, dek []byte) ([]byte, error) {
	gcm, err := newGCM(master)
	if err != nil {
		return nil, err
	}
	nonce, err := randBytes(gcm.NonceSize())
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, dek, nil), nil
}

// UnwrapKey 是 WrapKey 的逆操作。
func UnwrapKey(master, wrapped []byte) ([]byte, error) {
	gcm, err := newGCM(master)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < gcm.NonceSize() {
		return nil, errors.New("wrapped key too short")
	}
	nonce, ct := wrapped[:gcm.NonceSize()], wrapped[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

// Seal 用 key 加密一小段数据（AES-GCM），aad 绑定数据的用途和归属（如文件 ID），输出 nonce || ciphertext。
func Seal(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce, err := randBytes(gcm.NonceSize())
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// Open 是 Seal 的逆操作，aad 必须和加密时相同。
func Open(key, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("sealed data too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], aad)
}

// NewCTRAt 返回一个从 offset 字节处开始的 AES-CTR 流，等价于把从 0 开始的流丢弃前 offset 字节。
func NewCTRAt(key, iv []byte, offset int64) (cipher.Stream, error) {
	if len(iv) != IVSize {
		return nil, fmt.Errorf("iv must be %d bytes", IVSize)
	}
	if offset < 0 {
		return nil, errors.New("negative offset")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	ctr := addCounter(iv, uint64(offset/IVSize))
	stream := cipher.NewCTR(block, ctr)
	if skip := offset % IVSize; skip > 0 {
		var junk [IVSize]byte
		stream.XORKeyStream(junk[:skip], junk[:skip])
	}
	return stream, nil
}

// addCounter 把 iv 当作 128 位大端整数加上 n（与 crypto/cipher 的 CTR 递增方式一致）。
func addCounter(iv []byte, n uint64) []byte {
	out := make([]byte, len(iv))
	copy(out, iv)
	carry := n
	for i := len(out) - 1; i >= 0 && carry > 0; i-- {
		sum := uint64(out[i]) + (carry & 0xff)
		out[i] = byte(sum)
		carry = (carry >> 8) + (sum >> 8)
	}
	return out
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

package auth

import (
	"crypto/sha256"
	"strings"
)

// UploadKeyPrefix 是上传密钥的前缀，便于认出来（也便于密钥扫描工具识别）。
const UploadKeyPrefix = "xju_"

// NewUploadKey 生成一个只能上传的密钥（256 位随机），返回密钥和要保存的哈希。
func NewUploadKey() (key string, hash []byte, err error) {
	r, err := randomString(32)
	if err != nil {
		return "", nil, err
	}
	key = UploadKeyPrefix + r
	return key, HashUploadKey(key), nil
}

// HashUploadKey 计算密钥的哈希（密钥本身足够随机，不需要慢哈希）。
func HashUploadKey(key string) []byte {
	sum := sha256.Sum256([]byte("xiaoju upload key\x00" + strings.TrimSpace(key)))
	return sum[:]
}

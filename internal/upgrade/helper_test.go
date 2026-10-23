package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex 只给测试用：期望值必须来自独立来源（这里是对源字节现算），
// 不能让被测代码自己算一遍再自比。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

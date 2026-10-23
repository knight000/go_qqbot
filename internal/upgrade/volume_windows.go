//go:build windows

package upgrade

import (
	"path/filepath"
	"strings"
)

// sameVolume 在 Windows 上只能比盘符（VolumeName），拿不到设备号。
// 同一个盘符下 rename 一定是原子的目录项替换；跨盘符会失败而不是静默复制。
func sameVolume(a, b string) (bool, error) {
	va := filepath.VolumeName(abs(a))
	vb := filepath.VolumeName(abs(b))
	if va == "" || vb == "" {
		return false, nil
	}
	return strings.EqualFold(va, vb), nil
}

// syncDir 在 Windows 上不做任何事：没有目录 fsync 这个概念，
// MoveFileEx 的语义已经保证目录项更新（ADR-0006 决策 5）。
func syncDir(string) error { return nil }

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

//go:build !windows

package upgrade

import (
	"os"
	"syscall"
)

// sameVolume 在 Unix 上比设备号：两个路径在同一个 st_dev 上，rename 才是
// 原子的目录项替换，否则内核会退化成"复制 + 删除"。
func sameVolume(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	da, ok := sa.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	db, ok := sb.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return da.Dev == db.Dev, nil
}

// syncDir 把目录项刷到盘上。rename 之后不做这一步，掉电可能失去整个文件。
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

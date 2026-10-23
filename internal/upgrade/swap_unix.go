//go:build !windows

package upgrade

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Swap 把 <exe>.new 换成正式的 exe，并把旧映像留在 <exe>.old。
//
// 顺序（ADR-0006 决策 5）：硬链接旧的 exe → rename 新的上来。
// 用 link 而不是 rename 旧的走，是因为 rename(exe, exe.old) 会让 exe 在
// 一个窗口里不存在；link 之后 exe 仍指向旧 inode，直到下一句 rename 原子替换。
func Swap(exe string) error {
	old := OldPath(exe)
	staged := StagedPath(exe)
	if _, err := os.Stat(staged); err != nil {
		return fmt.Errorf("没有可换的产物 %s: %w", staged, err)
	}
	// 上次的残留（或上次启动没删干净的 .old）会让 link 直接失败。
	if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清理旧的 %s: %w", old, err)
	}
	if err := os.Link(exe, old); err != nil {
		// 部分文件系统不支持硬链接（FAT32/exFAT、某些网络盘）。退化成复制：
		// 慢，但保住"旧版本可回滚"这个前提，比直接失败好。
		if cerr := copyFile(exe, old); cerr != nil {
			return fmt.Errorf("备份旧二进制 %s: %w（硬链接失败: %v）", old, cerr, err)
		}
	}
	if err := os.Rename(staged, exe); err != nil {
		// 换盘没成 → 旧二进制还在原位，删掉半成品备份，保持目录干净。
		_ = os.Remove(old)
		return fmt.Errorf("替换 %s: %w", exe, err)
	}
	_ = syncDir(filepath.Dir(exe))
	return nil
}

// Restore 把 <exe>.old 换回 exe。回滚路径，失败就是"目录里没有可用的二进制"。
func Restore(exe string) error {
	old := OldPath(exe)
	if err := checkOldIsRegular(old); err != nil {
		return err
	}
	if err := os.Rename(old, exe); err != nil {
		return fmt.Errorf("回滚 %s: %w", exe, err)
	}
	_ = syncDir(filepath.Dir(exe))
	return nil
}

// copyFile 是硬链接不可用时的退路。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

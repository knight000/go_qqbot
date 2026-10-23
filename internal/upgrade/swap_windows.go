//go:build windows

package upgrade

import (
	"fmt"
	"os"
)

// Swap 在 Windows 上的顺序与 Unix 不同（ADR-0006 决策 5）：
// 运行中的 exe 不能覆盖、不能删除，但**可以改名**（镜像以 FILE_SHARE_DELETE
// 打开）。所以先 rename(exe → exe.old)，再 rename(exe.new → exe)。
//
// 第二句失败时立刻把 .old 改回来：这一步还在进程内（关停已完成、但进程
// 尚未退出），能收住就必须收住，否则目录里会没有 exe。
func Swap(exe string) error {
	old := OldPath(exe)
	staged := StagedPath(exe)
	if _, err := os.Stat(staged); err != nil {
		return fmt.Errorf("没有可换的产物 %s: %v", staged, err)
	}
	// 启动时清过，但杀软可能在这之后又把 .old 放回来/占住。
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("把运行中的 %s 改名为 %s: %v", exe, old, err)
	}
	if err := os.Rename(staged, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("替换 %s: %v（回滚也失败: %v）", exe, err, rerr)
		}
		return fmt.Errorf("替换 %s: %v（旧二进制已改回原名）", exe, err)
	}
	return nil
}

// Restore 把 <exe>.old 换回 exe。
func Restore(exe string) error {
	old := OldPath(exe)
	if err := checkOldIsRegular(old); err != nil {
		return err
	}
	if err := os.Rename(old, exe); err != nil {
		return fmt.Errorf("回滚 %s: %v", exe, err)
	}
	return nil
}

//go:build windows

package main

import (
	"os"
	"os/exec"
)

// restartSelf 在 Windows 上只能"拉起新进程 + 自己退出"（没有 execve）。
//
// 安全性来自调用时机：run 返回时网关已断开、session 已落盘、SQLite 已关闭，
// 新进程起来时不会和旧进程抢同一个 gateway session 与数据库写锁。
// 子进程继承控制台，前台运行时输出仍然可见。
func restartSelf(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	cmd.Env = withRestartEnv(os.Environ())
	if err := cmd.Start(); err != nil {
		return err
	}
	// Release 让子进程脱离 Go 的进程管理：不留下句柄与等待中的僵尸记录。
	return cmd.Process.Release()
}

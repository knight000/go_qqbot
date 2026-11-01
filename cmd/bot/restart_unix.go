//go:build !windows

package main

import (
	"os"
	"syscall"
)

// restartSelf 用 execve 原地替换进程映像：PID 不变，systemd 的 cgroup、
// 已打开的日志句柄与控制台都沿用原来的，不会出现"父进程退出把子进程一起收走"。
// 成功时不返回；失败返回错误，由调用方退出。
//
// reason 会写进 QQBOT_RESTARTED，新进程据此在日志与启动通知里说明"为什么重启"。
func restartSelf(exe, reason string) error {
	argv := append([]string{exe}, os.Args[1:]...)
	return syscall.Exec(exe, argv, withRestartEnv(os.Environ(), reason))
}

//go:build !windows

package main

import (
	"os"
	"syscall"
)

// restartSelf 用 execve 原地替换进程映像：PID 不变，systemd 的 cgroup、
// 已打开的日志句柄与控制台都沿用原来的，不会出现"父进程退出把子进程一起收走"。
// 成功时不返回；失败返回错误，由调用方退出。
func restartSelf(exe string) error {
	argv := append([]string{exe}, os.Args[1:]...)
	return syscall.Exec(exe, argv, withRestartEnv(os.Environ()))
}

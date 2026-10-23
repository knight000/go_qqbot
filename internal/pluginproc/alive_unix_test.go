//go:build !windows

package pluginproc_test

import "syscall"

// processAlive 报告 pid 是否还活着：Signal(0) 是"只做存在性检查、不真的发信号"。
// ESRCH 表示进程不存在；EPERM 表示存在但不属于当前用户——同样算活着。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

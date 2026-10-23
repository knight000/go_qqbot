//go:build !windows

package pluginproc

import (
	"os"
	"syscall"
)

// terminate 发 SIGTERM：让插件有机会自己收尾（关协程、落盘），
// 而不是像 Kill 那样被平台直接抹掉。
func terminate(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

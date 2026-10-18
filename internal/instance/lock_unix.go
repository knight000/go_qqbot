//go:build unix

package instance

import (
	"errors"
	"os"
	"syscall"
)

// lockFile 用 flock 锁住这个打开文件描述（不是进程，也不是路径）。
//
// 选 flock 而不是"锁文件是否存在"，是因为前者在进程被 kill -9 时由内核释放，
// 后者会留下一个需要人工判断"这个文件代表还活着，还是上次崩了"的残留物。
//
// 注意：正常退出路径会显式 Release，/reload 也是先放手再 execve，所以新映像
// 重新加锁时不会撞上自己。另外 Go 的 os.OpenFile 带 O_CLOEXEC，就算将来有谁
// 跳过 Release 直接 exec，锁也不会被继承进新进程映像——两层保险，互不依赖。
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	// LOCK_NB：拿不到就立刻返回，是否重试由 Acquire 决定。
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errBusy
		}
		return nil, err
	}
	return f, nil
}

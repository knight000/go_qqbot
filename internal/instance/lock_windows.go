//go:build windows

package instance

import (
	"errors"
	"os"
	"syscall"
)

// errorSharingViolation 是 ERROR_SHARING_VIOLATION（32）。
// syscall 包没有导出这个常量，只能自己写；数值来自 WinError.h，稳定不变。
const errorSharingViolation syscall.Errno = 32

// lockFile 用 CreateFile 的共享模式当锁：以"读写访问 + 只允许别人读"打开文件，
// 第二个想同样打开的进程会拿到 ERROR_SHARING_VIOLATION。
//
// 之所以留 FILE_SHARE_READ 而不是 0：失败的那个进程需要能读回锁文件里的 pid，
// 否则错误消息只能说"被占用"，用户没法知道该结束哪个进程。共享读不会让两个
// 实例同时拿到锁——双方请求的都是写访问，而各自的 dwShareMode 都不含
// FILE_SHARE_WRITE，两边都过不了共享检查。
//
// 选 CreateFile 而不是 LockFileEx：句柄随进程结束由内核关闭，锁自动释放，
// 没有"崩溃后残留锁"的问题，代码也只有一次调用。
func lockFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, errBusy
		}
		return nil, os.NewSyscallError("CreateFile", err)
	}
	return os.NewFile(uintptr(h), path), nil
}

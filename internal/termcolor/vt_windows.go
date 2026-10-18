//go:build windows

package termcolor

import (
	"os"
	"syscall"
	"unsafe"
)

const enableVirtualTerminalProcessing = 0x0004

// enableVT 给控制台打开 VT 转义解析。
//
// Windows 的 conhost 默认不解释 ANSI：不开这个标志，`\x1b[33m` 这类序列会被
// 原样打印成 `←[33m`。Windows Terminal / VSCode 内置终端同样走这条路径，
// 只是它们本来就能解析。拿不到控制台句柄（重定向、被别的进程接管）时返回
// false，调用方退回无色——上色失败不该影响任何逻辑。
func enableVT(f *os.File) bool {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	var mode uint32
	h := f.Fd()
	if r, _, _ := getConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	r, _, _ := setConsoleMode.Call(h, uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}

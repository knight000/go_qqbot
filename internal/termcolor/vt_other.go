//go:build !windows

package termcolor

import "os"

// enableVT 在 POSIX 上是空操作：终端本来就能解析 ANSI。
// 保留同名函数是为了让 Supported 不必区分平台。
func enableVT(*os.File) bool { return true }

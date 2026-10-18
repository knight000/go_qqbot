//go:build !unix && !windows

package instance

import "os"

// lockFile 在不支持的平台上退化为不加锁。
//
// 这些目标（plan9 / js / wasip1）本来也跑不了这个机器人，编译通过比一层
// 似有实无的保护更有价值。
func lockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}

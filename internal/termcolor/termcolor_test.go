package termcolor

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// 非 *os.File 的 writer 一律不上色：测试替身、网络连接都不该收转义码。
func TestSupportedRejectsNonFile(t *testing.T) {
	var buf bytes.Buffer
	if Supported(&buf) {
		t.Fatal("bytes.Buffer 不应被判为终端")
	}
}

// 重定向到文件是"落盘"场景：必须是纯文本，否则日志没法 grep。
func TestSupportedRejectsRegularFile(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out.log"))
	if err != nil {
		t.Fatalf("创建文件: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if Supported(f) {
		t.Fatal("普通文件不应被判为终端")
	}
}

// NO_COLOR 是跨工具的约定：设了就不上色，即使真的在终端里。
func TestSupportedHonoursNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if Supported(os.Stdout) {
		t.Fatal("设置 NO_COLOR 后不应上色")
	}
}

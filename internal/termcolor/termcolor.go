// Package termcolor 判断输出能不能带 ANSI 颜色，并给出固定色码。
//
// 判定刻意保守：要的是"终端里能看见颜色、落盘后没有垃圾字符"，
// 而不是"尽可能上色"。所以重定向到文件、写进 journald、测试用的
// buffer 一律返回 false。
package termcolor

import (
	"io"
	"os"
)

// 固定色码。只给日志里"需要一眼看到"的少数位置上色：
// 轨迹时间戳（青蓝）、WARN（黄）、ERROR（红）。不做主题，不做开关。
const (
	Reset  = "\x1b[0m"
	Cyan   = "\x1b[36m"
	Yellow = "\x1b[33m"
	Red    = "\x1b[31m"
)

// Supported 报告往 w 写 ANSI 是否安全，按顺序判断：
//
//   - 只认 *os.File：bytes.Buffer、测试替身一律纯文本；
//   - 设了 NO_COLOR 时关闭（https://no-color.org 的约定）；
//   - 普通文件（`> log.txt`）不上色，日志落盘后可以直接读；
//   - 字符设备是真控制台，交给平台判断（Windows 需要先打开 VT 解析）；
//   - 命名管道：Git Bash / mintty 这类伪终端下，原生 Windows 程序的 stdout
//     就是管道，终端仿真器在另一端解析 ANSI。用 TERM 区分它和
//     systemd/journald 那种没有终端的管道——后者不该收转义码，
//     而 TERM=dumb 明确表示"不认转义序列"，同样排除。
func Supported(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode().IsRegular() {
		return false
	}
	if fi.Mode()&os.ModeCharDevice != 0 {
		return enableVT(f)
	}
	term := os.Getenv("TERM")
	return term != "" && term != "dumb"
}

// Wrap 在 s 两侧加色码；code 取本包的常量。
func Wrap(code, s string) string { return code + s + Reset }

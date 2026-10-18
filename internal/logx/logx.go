// Package logx 统一日志构造。标准库 slog 足够，不引入第三方日志库。
package logx

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/knight000/go_qqbot/internal/termcolor"
)

// New 按配置构造 JSON 或文本 handler。生产建议 JSON，交给 journald。
//
// 文本格式在终端里给 WARN 上黄、ERROR 上红：日志大部分是 INFO，
// 出问题时眼睛要能直接落到那两行上。JSON 永不上色——它的读者是机器。
func New(level, format string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}

	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
		return slog.New(h)
	}
	// 只在这个 writer 上着色：判定见 termcolor.Supported（重定向到文件时自动退回纯文本）。
	var w io.Writer = os.Stdout
	if termcolor.Supported(os.Stdout) {
		w = levelColorWriter{w: os.Stdout}
	}
	h = slog.NewTextHandler(w, opts)
	return slog.New(h)
}

// levelColorWriter 给文本日志的 level 字段上色。
//
// 为什么不在 HandlerOptions.ReplaceAttr 里做：TextHandler 对含控制字符的
// 字符串值会加引号并用 strconv 转义，ReplaceAttr 返回带 ANSI 的值时得到的是
// `level="\x1b[33mWARN\x1b[0m"` 这样的字面量，反而更难读。
//
// 走写端则很简单：TextHandler 每条记录只调用一次 Write，所以这里拿到的
// 就是完整的一行，替换行首那个固定的 `level=WARN` / `level=ERROR` 即可。
type levelColorWriter struct{ w io.Writer }

func (c levelColorWriter) Write(p []byte) (int, error) {
	if _, err := c.w.Write(colorizeLevel(p)); err != nil {
		return 0, err
	}
	return len(p), nil // 按输入长度计数：调用方不关心实际写出的字节数
}

// colorizeLevel 把 level 值包上颜色。
//
// 只在 `msg=` 之前找：TextHandler 的顺序固定是 `time=… level=… msg=…`，
// 正文再怎么写都不会落到这个前缀里。不按字节偏移猜，是因为时间的字面长度
// 会随时区与精度变化，而 `msg=` 的位置与字段顺序是 slog 的固定行为。
func colorizeLevel(p []byte) []byte {
	head := p
	if i := bytes.Index(p, []byte(" msg=")); i >= 0 {
		head = p[:i]
	}
	for _, c := range [...]struct {
		level string
		color string
	}{
		{"ERROR", termcolor.Red},
		{"WARN", termcolor.Yellow},
	} {
		old := []byte("level=" + c.level)
		i := bytes.Index(head, old)
		if i < 0 {
			continue
		}
		out := make([]byte, 0, len(p)+len(c.color)+len(termcolor.Reset))
		out = append(out, p[:i]...)
		out = append(out, "level="...)
		out = append(out, c.color...)
		out = append(out, c.level...)
		out = append(out, termcolor.Reset...)
		out = append(out, p[i+len(old):]...)
		return out
	}
	return p
}

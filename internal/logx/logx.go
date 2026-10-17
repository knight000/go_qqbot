// Package logx 统一日志构造。标准库 slog 足够，不引入第三方日志库。
package logx

import (
	"log/slog"
	"os"
	"strings"
)

// New 按配置构造 JSON 或文本 handler。生产建议 JSON，交给 journald。
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
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

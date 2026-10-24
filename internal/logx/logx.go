// Package logx 统一日志构造。标准库 slog 足够，不引入第三方日志库。
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/knight000/go_qqbot/internal/termcolor"
)

// New 按配置构造 JSON 或文本 handler。生产建议 JSON，交给 journald。
//
// 文本格式为 `[HH:MM:SS] [LEVEL] msg k=v`，与调试轨迹共用同一套行首外观：
// 终端里时间戳青蓝、WARN 黄、ERROR 红；重定向到文件或设了 NO_COLOR 时
// 全部退回纯文本。JSON 永不上色——它的读者是机器。
func New(level, format string) *slog.Logger {
	lv := parseLevel(level)
	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
	}
	return slog.New(newTextHandler(os.Stdout, lv, termcolor.Supported(os.Stdout)))
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// textHandler 输出 `[HH:MM:SS] [LEVEL] msg k=v`。
//
// 为什么不用 slog.TextHandler：它的骨架固定是 `time=… level=… msg=…`，
// ReplaceAttr 只能改值、改不了键名与顺序，msg 的引号也由内部 needsQuoting
// 决定。轨迹那边已经用 `[HH:MM:SS]` 开头，日志跟它对齐后一眼能看出是
// 同一条链路上的输出。自研的代价是 attrs 的转义规则要自己维护，所以
// 这里刻意照着 TextHandler 的规则来（只有必要时才加引号）。
type textHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Level
	color bool
	// prefix 是 WithAttrs 预渲染好的片段，已按当时的 group 上下文定格。
	// group 只影响"之后"的 attrs——与 slog.TextHandler 的分组语义一致：
	// With("a",1).WithGroup("g").With("b",2) 得到 `a=1 g.b=2`。
	prefix []string
	group  string
}

func newTextHandler(w io.Writer, level slog.Level, color bool) *textHandler {
	return &textHandler{mu: new(sync.Mutex), w: w, level: level, color: color}
}

func (h *textHandler) Enabled(_ context.Context, lv slog.Level) bool { return lv >= h.level }

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	cp := *h
	cp.prefix = slices.Clone(h.prefix)
	collect(h.group, attrs, &cp.prefix)
	return &cp
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	cp := *h
	if cp.group == "" {
		cp.group = name
	} else {
		cp.group += "." + name
	}
	return &cp
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(h.timeTag(r.Time))
	b.WriteByte(' ')
	b.WriteString(h.levelTag(r.Level))
	if r.Message != "" {
		b.WriteByte(' ')
		b.WriteString(quoteMessage(r.Message))
	}

	// 每个片段自带 `key=value`，用空格拼起来；空 key 的内联值只有 value。
	parts := slices.Clone(h.prefix)
	r.Attrs(func(a slog.Attr) bool {
		collect(h.group, []slog.Attr{a}, &parts)
		return true
	})
	if len(parts) > 0 {
		b.WriteByte(' ')
		b.WriteString(strings.Join(parts, " "))
	}
	b.WriteByte('\n')

	// 一条记录一次写入，多协程下不会交错。
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *textHandler) timeTag(t time.Time) string {
	s := "[" + t.Format("15:04:05") + "]"
	if !h.color {
		return s
	}
	return termcolor.Wrap(termcolor.Cyan, s)
}

// levelTag 把等级写成固定四档的 `[INFO]`：自定义等级（INFO+1 之类）按区间归并，
// 避免出现 `[INFO+2]` 这种在控制台里长短不一的标签。
func (h *textHandler) levelTag(lv slog.Level) string {
	name, color := "DEBUG", ""
	switch {
	case lv >= slog.LevelError:
		name, color = "ERROR", termcolor.Red
	case lv >= slog.LevelWarn:
		name, color = "WARN", termcolor.Yellow
	case lv >= slog.LevelInfo:
		name = "INFO"
	}
	s := "[" + name + "]"
	if h.color && color != "" {
		return termcolor.Wrap(color, s)
	}
	return s
}

// collect 把一个 attr 展开成若干 `key=value` 片段。
// 组按 slog 的约定压成点号前缀；空 key 的 attr 只输出值（如 `slog.Any("", err)`）。
func collect(group string, attrs []slog.Attr, out *[]string) {
	for _, a := range attrs {
		a.Value = a.Value.Resolve()
		if a.Equal(slog.Attr{}) {
			continue // 零值 attr 由 slog 约定忽略
		}
		if a.Value.Kind() == slog.KindGroup {
			g := a.Value.Group()
			if len(g) == 0 {
				continue
			}
			inner := group
			if a.Key != "" {
				if inner != "" {
					inner += "."
				}
				inner += a.Key
			}
			collect(inner, g, out)
			continue
		}

		var s strings.Builder
		if a.Key != "" {
			if group != "" {
				s.WriteString(group)
				s.WriteByte('.')
			}
			s.WriteString(a.Key)
			s.WriteByte('=')
		}
		s.WriteString(formatValue(a.Value))
		*out = append(*out, s.String())
	}
}

func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return quote(v.String())
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	default:
		return quote(fmt.Sprintf("%+v", v.Any()))
	}
}

// quote 只在必要时加引号，规则与 slog.TextHandler 一致：
// 空串、含空白/等号/引号/控制字符、或不是合法 UTF-8 时才转义。
// 这样 `role=bot-admin`、`peer=g1` 保持可 grep，`err="i/o timeout"` 也不会歧义。
func quote(s string) string {
	if !needsQuoting(s) {
		return s
	}
	return strconv.Quote(s)
}

func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			if b == ' ' || b == '=' || b == '"' || b < ' ' || b == 0x7f {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return true
		}
		i += size
	}
	return false
}

// quoteMessage 只在出现控制字符时转义：正文里的空格是正常的，不该被括起来；
// 但换行会把一条记录拆成两行，那种情况必须转义成 `\n`。
func quoteMessage(s string) string {
	if !utf8.ValidString(s) {
		return strconv.Quote(s)
	}
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			return strconv.Quote(s)
		}
	}
	return s
}

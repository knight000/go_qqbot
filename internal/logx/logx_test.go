package logx

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/termcolor"
)

var testTime = time.Date(2026, 10, 19, 13, 50, 33, 666e6, time.Local)

func record(msg string, lv slog.Level, attrs ...slog.Attr) slog.Record {
	r := slog.NewRecord(testTime, lv, msg, 0)
	r.AddAttrs(attrs...)
	return r
}

// 文本日志的骨架：`[HH:MM:SS] [LEVEL] msg k=v`，正文不因为含空格被加引号。
func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandler(&buf, slog.LevelInfo, false)
	r := record("adminping 插件已初始化", slog.LevelInfo,
		slog.String("plugin", "adminping"), slog.String("role", "bot-admin"))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := "[13:50:33] [INFO] adminping 插件已初始化 plugin=adminping role=bot-admin\n"
	if got := buf.String(); got != want {
		t.Fatalf("格式不符\n期望 %q\n实际 %q", want, got)
	}
}

// 值里的空格要加引号，否则 `err=i/o timeout` 会被读成两个字段。
func TestTextFormatQuotesValues(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandler(&buf, slog.LevelInfo, false)
	if err := h.Handle(context.Background(), record("调用失败", slog.LevelWarn,
		slog.String("err", "i/o timeout"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if want := `err="i/o timeout"`; !strings.Contains(buf.String(), want) {
		t.Fatalf("期望包含 %q，实际 %q", want, buf.String())
	}
}

// 终端里时间戳青蓝、WARN 黄、ERROR 红；INFO 的等级标签不上色。
func TestTextColors(t *testing.T) {
	cases := []struct {
		name  string
		level slog.Level
		msg   string
		tag   string // 已上色的等级标签
	}{
		{"WARN 黄", slog.LevelWarn, "队列已满", termcolor.Wrap(termcolor.Yellow, "[WARN]")},
		{"ERROR 红", slog.LevelError, "连接失败", termcolor.Wrap(termcolor.Red, "[ERROR]")},
		{"INFO 只有时间是彩色", slog.LevelInfo, "普通", "[INFO]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := newTextHandler(&buf, slog.LevelDebug, true)
			if err := h.Handle(context.Background(), record(c.msg, c.level)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			want := termcolor.Wrap(termcolor.Cyan, "[13:50:33]") + " " + c.tag + " " + c.msg + "\n"
			if got := buf.String(); got != want {
				t.Fatalf("上色不符\n期望 %q\n实际 %q", want, got)
			}
		})
	}
}

// WithAttrs 在 WithGroup 之前定格，之后的 attrs 才带组前缀——与 TextHandler 一致。
func TestTextHandlerGroups(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandler(&buf, slog.LevelInfo, false).
		WithAttrs([]slog.Attr{slog.String("component", "gateway")}).
		WithGroup("db")
	h2 := h.WithAttrs([]slog.Attr{slog.Int("rows", 3)})
	if err := h2.Handle(context.Background(), record("查询完成", slog.LevelInfo, slog.Bool("ok", true))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := "[13:50:33] [INFO] 查询完成 component=gateway db.rows=3 db.ok=true\n"
	if got := buf.String(); got != want {
		t.Fatalf("分组格式不符\n期望 %q\n实际 %q", want, got)
	}
}

// 换行必须转义，否则一条记录会被拆成两行。
func TestTextFormatQuotesMultilineMessage(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandler(&buf, slog.LevelInfo, false)
	if err := h.Handle(context.Background(), record("第一行\n第二行", slog.LevelInfo)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if want := `"第一行\n第二行"`; !strings.Contains(buf.String(), want) {
		t.Fatalf("多行正文应被转义，期望包含 %q，实际 %q", want, buf.String())
	}
	if strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("一条记录只能占一行，实际 %q", buf.String())
	}
}

// 空消息不留多余空格：`[13:50:33] [INFO] k=v`。
func TestTextFormatEmptyMessage(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandler(&buf, slog.LevelInfo, false)
	if err := h.Handle(context.Background(), record("", slog.LevelInfo, slog.String("k", "v"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if want := "[13:50:33] [INFO] k=v\n"; buf.String() != want {
		t.Fatalf("空消息格式不符\n期望 %q\n实际 %q", want, buf.String())
	}
}

// 等级过滤：低于配置等级的记录直接不写。
func TestTextHandlerEnabled(t *testing.T) {
	h := newTextHandler(new(bytes.Buffer), slog.LevelWarn, false)
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatalf("warn 级别不应放行 info")
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) || !h.Enabled(context.Background(), slog.LevelError) {
		t.Fatalf("warn 级别应放行 warn 与 error")
	}
}

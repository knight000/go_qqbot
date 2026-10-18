package logx

import (
	"bytes"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/termcolor"
)

// 上色只认行首那个 level 字段：正文里出现同样的字样不能误伤。
func TestColorizeLevel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 空表示"原样返回"
	}{
		{
			name: "WARN 黄",
			in:   `time=2026-10-19T01:00:00.000+08:00 level=WARN msg="队列已满"`,
			want: `time=2026-10-19T01:00:00.000+08:00 level=` + termcolor.Yellow + `WARN` + termcolor.Reset + ` msg="队列已满"`,
		},
		{
			name: "ERROR 红",
			in:   `time=2026-10-19T01:00:00.000+08:00 level=ERROR msg="连接失败" err="i/o timeout"`,
			want: `time=2026-10-19T01:00:00.000+08:00 level=` + termcolor.Red + `ERROR` + termcolor.Reset + ` msg="连接失败" err="i/o timeout"`,
		},
		{
			name: "INFO 不动",
			in:   `time=2026-10-19T01:00:00.000+08:00 level=INFO msg=ok`,
		},
		{
			name: "DEBUG 不动",
			in:   `time=2026-10-19T01:00:00.000+08:00 level=DEBUG msg=ok`,
		},
		{
			name: "正文里的 level=ERROR 不误伤",
			in:   `time=2026-10-19T01:00:00.000+08:00 level=INFO msg="他说 level=ERROR 才是问题"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := c.want
			if want == "" {
				want = c.in
			}
			if got := string(colorizeLevel([]byte(c.in))); got != want {
				t.Fatalf("上色结果不符\n期望 %q\n实际 %q", want, got)
			}
		})
	}
}

// Write 必须按输入长度计数：TextHandler 依赖 io.Writer 的契约，
// 返回实际写出的字节数（多了几个转义码）会被当成短写。
func TestLevelColorWriterCountsInputBytes(t *testing.T) {
	var buf bytes.Buffer
	in := []byte("time=x level=WARN msg=w\n")
	n, err := levelColorWriter{w: &buf}.Write(in)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(in) {
		t.Fatalf("应返回输入长度 %d，实际 %d", len(in), n)
	}
	if !strings.Contains(buf.String(), termcolor.Yellow+"WARN"+termcolor.Reset) {
		t.Fatalf("输出未上色: %q", buf.String())
	}
}

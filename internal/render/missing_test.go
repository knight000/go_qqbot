package render

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// 子集外的生僻字画方框并打一条 warn，但不是渲染失败——出一张"有个字是
// 方框"的图，比让用户什么都看不到更接近意图。
func TestMissingGlyphWarnsButRenders(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r := New(testRenderer(t).set, log)

	// U+2000B 在 CJK 扩展 B 区，GB2312 子集里没有。
	src := "生僻字 𠀋 与正常文字混排。"
	img, err := r.Markdown(context.Background(), src)
	if err != nil {
		t.Fatalf("缺字不应让渲染失败：%v", err)
	}
	if !img.Valid() {
		t.Fatal("Image.Valid() = false，期望 true")
	}
	data := img.Data()
	m := decodePNG(t, data)
	if m.Bounds().Dx() != int(imgWidth) || m.Bounds().Dy() <= 0 {
		t.Errorf("尺寸 = %v，期望宽 %v、高 > 0", m.Bounds(), imgWidth)
	}

	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "方框") {
		t.Fatalf("日志里没有缺字 warn：%q", out)
	}
	// 一条 warn 覆盖整篇：一行生僻字不该刷出几十条日志。
	if n := strings.Count(out, "msg="); n != 1 {
		t.Errorf("warn 条数 = %d，期望 1（日志：%q）", n, out)
	}

	writeSample(t, "missing.png", data)
}

// 正常中文不该触发缺字 warn：否则每次渲染都在报警，等于没有报警。
func TestKnownGlyphsDoNotWarn(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r := New(testRenderer(t).set, log)

	if _, err := r.Markdown(context.Background(), "常用汉字与 ASCII 都应完整显示。"); err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	if out := buf.String(); out != "" {
		t.Errorf("正常内容产生了 warn：%q", out)
	}
}

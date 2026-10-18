package render

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// 这三族的 cmap 互补：Noto Sans 没有 → ✓ ★ ● 与全角括号，Noto Sans SC 全都有。
// 逐字符回退链必须把它们送到 SC，而不是在首选族（Sans）上判成缺字。
const fallbackRunes = "→✓★●「」"

// TestFallbackRunesRenderWithoutWarn 是本特性的主用例：状态表里的 → ✓ ★ 必须
// 真的画出来，而不是"方框 + 一条 warn"。
func TestFallbackRunesRenderWithoutWarn(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r := New(testRenderer(t).set, log)

	src := "状态：构建 → 部署 ✓，评级 ★★★，分隔 ●，引号「示例」。\n\n" +
		"| 阶段 | 结果 |\n| --- | --- |\n| 构建 | 通过 ✓ ★ ● |\n| 部署 | A → B 「预发」 |\n"

	img, err := r.Markdown(context.Background(), src)
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	data := img.Data()
	m := decodePNG(t, data)
	if m.Bounds().Dy() <= 0 {
		t.Fatalf("出图高度 = %d，期望 > 0", m.Bounds().Dy())
	}

	if out := buf.String(); out != "" {
		t.Errorf("子集内字符被误判成缺字，产生了 warn：%q", out)
	}

	writeSample(t, "fallback-runes.png", data)
}

// TestFallbackPicksFaceOfChosenFamily 是白盒断言：这些字符最终的 face 必须
// 属于真正有字形的族（SC），推进量也取自那个 face。
func TestFallbackPicksFaceOfChosenFamily(t *testing.T) {
	faces := testRenderer(t).faces.scope()
	defer faces.release()

	l := newLayout(faces, "")
	gs, err := l.glyphs([]span{{text: fallbackRunes}}, WeightRegular, fsBody)
	if err != nil {
		t.Fatalf("glyphs: %v", err)
	}
	if len(gs) != len([]rune(fallbackRunes)) {
		t.Fatalf("字形数 = %d，期望 %d", len(gs), len([]rune(fallbackRunes)))
	}

	want, err := faces.face(FontSC, WeightRegular, fsBody)
	if err != nil {
		t.Fatalf("face(FontSC): %v", err)
	}
	for _, g := range gs {
		if g.missing {
			t.Errorf("%q 被判成缺字，期望回退到 SC", g.r)
			continue
		}
		if g.f != want {
			t.Errorf("%q 的 face 不是 FontSC", g.r)
		}
		adv, ok := want.GlyphAdvance(g.r)
		if !ok {
			t.Fatalf("SC 也没有 %q 的字形，测试前提不成立", g.r)
		}
		if got := float64(adv) / 64; g.adv != got {
			t.Errorf("%q 的推进量 = %v，期望取自 SC 的 %v", g.r, g.adv, got)
		}
	}
	if len(l.missing) != 0 {
		t.Errorf("缺字表 = %v，期望为空", l.missing)
	}
}

// 代码块里的 → 与正文同理：mono 缺字形时也要回退，而不是只认 FontMono。
func TestFallbackAppliesToCodeRuns(t *testing.T) {
	faces := testRenderer(t).faces.scope()
	defer faces.release()

	l := newLayout(faces, "")
	gs, err := l.glyphs([]span{{text: "a → b ✓", mono: true}}, WeightRegular, fsCode)
	if err != nil {
		t.Fatalf("glyphs: %v", err)
	}
	for _, g := range gs {
		if g.missing {
			t.Errorf("代码块里的 %q 被判成缺字", g.r)
		}
	}
	// ASCII 仍走等宽族：回退只补缺，不该把整段代码换成比例字体。
	for _, g := range gs {
		if g.r == 'a' || g.r == 'b' {
			mono, err := faces.face(FontMono, WeightRegular, fsCode)
			if err != nil {
				t.Fatalf("face(FontMono): %v", err)
			}
			if g.f != mono {
				t.Errorf("%q 的 face 不是 FontMono", g.r)
			}
		}
	}
}

// countingHandler 数 warn 记录条数：断言"同一 rune 一次渲染只 warn 一次"。
type countingHandler struct {
	n int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.n++
	}
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// 三族都没有的字符（CJK 扩展 B）仍画方框，但整篇只 warn 一次，且不算失败。
func TestMissingEverywhereWarnsOnce(t *testing.T) {
	h := &countingHandler{}
	r := New(testRenderer(t).set, slog.New(h))

	src := "扩展区 𠀀 𠀁 𠀂 连写多行\n\n| 列 |\n| --- |\n| 𠀀 |\n"
	img, err := r.Markdown(context.Background(), src)
	if err != nil {
		t.Fatalf("缺字不应让渲染失败：%v", err)
	}
	m := decodePNG(t, img.Data())
	if m.Bounds().Dy() <= 0 {
		t.Fatalf("出图高度 = %d，期望 > 0", m.Bounds().Dy())
	}
	if h.n != 1 {
		t.Errorf("warn 条数 = %d，期望 1", h.n)
	}

	// 日志里应当列出三个扩展区字符（同一个 rune 只列一次）。
	var buf bytes.Buffer
	r2 := New(testRenderer(t).set, slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelWarn})))
	if _, err := r2.Markdown(context.Background(), "𠀀 𠀀 𠀀"); err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "U+20000") || !strings.Contains(out, "count=1") {
		t.Errorf("warn 内容 = %q，期望只含一个 U+20000", out)
	}
}

package render

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/yuin/goldmark/text"
)

// inlineSample 是行内样式的观感基准：同一行里混排加粗、斜体、行内代码、
// 链接与删除线，用来肉眼确认药丸不压字。
const inlineSample = "本次构建覆盖 **3 个平台**，其中 `linux/amd64` 与 `windows/amd64` 通过，" +
	"*linux/arm64* 因交叉编译链缺失而~~跳过~~，详细产物见 [发布页](https://example.com/releases)。"

// 行内代码与链接的文本必须真的出现在行内：内容变长时行数跟着变。
//
// 只断言高度（外部可观察），不比像素。行内代码此前只走默认递归，CodeSpan
// 没有子节点，文本会整段消失——高度不增长正是这个 bug 的可观察面。
func TestInlineCodeAndLinkTextIsLaidOut(t *testing.T) {
	height := func(src string) int {
		img, err := testRenderer(t).Markdown(context.Background(), src)
		if err != nil {
			t.Fatalf("Markdown(%q): %v", src, err)
		}
		return decodePNG(t, img.Data()).Bounds().Dy()
	}

	base := height("占位")
	longCode := height("占位 " + strings.Repeat("`"+strings.Repeat("a", 200)+"` ", 4))
	if longCode <= base {
		t.Errorf("超长行内代码高度 %d 不大于单行 %d，行内代码文本很可能没有被布局", longCode, base)
	}

	// 链接要渲染成「文字 (url)」：长 URL 会把行撑开。
	plain := height("[发布页](x)")
	withURL := height("[发布页](https://example.com/" + strings.Repeat("releases/", 8) + ")")
	if withURL <= plain {
		t.Errorf("长 URL 高度 %d 不大于纯标签 %d，链接地址很可能没有被渲染成「文字 (url)」", withURL, plain)
	}
}

// 行内路径必须和代码块一样预处理 tab 与控制字符：字体里没有 tab 字形，
// 不展开会画成缺字方框，还会让缺字 warn 误报"字体子集外"；C0 控制字符
// 根本没有可画的形态，连推进量都不该占。
func TestInlineTabAndControlCharsArePreprocessed(t *testing.T) {
	l := newLayout(testRenderer(t).faces.scope(), "")
	gs, err := l.glyphs([]span{{text: "a\tb\x00c\x07d"}}, WeightRegular, fsBody)
	if err != nil {
		t.Fatalf("glyphs: %v", err)
	}

	var got []rune
	for _, g := range gs {
		if g.missing {
			t.Errorf("%q 被画成缺字方框，期望 tab/控制字符已被预处理", g.r)
		}
		got = append(got, g.r)
	}
	// tab → 4 个空格；\x00 与 \x07 整条丢弃。
	if want := "a    bcd"; string(got) != want {
		t.Errorf("字形序列 = %q，期望 %q", string(got), want)
	}
	if len(l.missing) != 0 {
		t.Errorf("缺字表 = %v，期望空：tab 与控制字符不是缺字", l.missing)
	}

	// 展开出来的空格要真的占宽度，否则"对齐用的 tab"会静默塌掉。
	spaces := 0.0
	for _, g := range gs {
		if g.r == ' ' {
			spaces += g.adv
		}
	}
	if spaces <= 0 {
		t.Error("tab 展开后的空格推进量为 0，对齐信息丢了")
	}
}

// 折行把行内代码 span 切成多行后，**每行**行首的字形都要带药丸的左内边距：
// 只在 span 首个字形上绑 padL 时，续行的药丸左缘会压在首字上（视觉裁切）。
// padding 同时必须计入推进量，否则文字会跑出药丸的右缘。
func TestCodeSpanContinuationLinesKeepLeftPad(t *testing.T) {
	src := "前置文字 " + "`" + strings.Repeat("code ", 40) + "`"
	l := newLayout(testRenderer(t).faces.scope(), src)
	doc := markdown.Parser().Parse(text.NewReader([]byte(src)))
	gs, err := l.glyphs(collectSpans(doc.FirstChild(), l.src), WeightRegular, fsBody)
	if err != nil {
		t.Fatalf("glyphs: %v", err)
	}

	maxW := l.right - l.x
	pad := inlinePad()
	continuations := 0
	for i, ln := range l.wrap(gs, maxW) {
		if i == 0 || len(ln) == 0 || !ln[0].code {
			continue
		}
		continuations++
		first := ln[0]
		if first.padL != pad {
			t.Errorf("第 %d 行（代码续行）行首 padL = %v，期望 %v", i, first.padL, pad)
			continue
		}
		if adv, ok := first.f.GlyphAdvance(first.r); ok {
			if want := float64(adv)/64 + pad; math.Abs(first.adv-want) > 0.01 {
				t.Errorf("第 %d 行行首 adv = %.2f，期望 %.2f（字形推进量 + 药丸 padding）",
					i, first.adv, want)
			}
		}
	}
	if continuations < 2 {
		t.Fatalf("只有 %d 行代码续行，样例不够长，测不出折行后的 padding", continuations)
	}
}

// 五种行内样式混排在同一行：不报错、宽度仍是基线、能解出 PNG。
func TestInlineStylesRenderTogether(t *testing.T) {
	data := renderPNG(t, inlineSample+"\n\n同一行再来一次：**粗** *斜* `code` [链接](https://example.com/a) ~~删~~。")

	m := decodePNG(t, data)
	if got := m.Bounds().Dx(); got != int(imgWidth) {
		t.Errorf("宽 = %d，期望 %v", got, imgWidth)
	}
	if h := m.Bounds().Dy(); h <= 0 {
		t.Fatalf("高 = %d，期望 > 0", h)
	}

	writeSample(t, "inline.png", data)
}

package render

import (
	"context"
	"strings"
	"testing"
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

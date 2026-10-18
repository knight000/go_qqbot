package render

import (
	"context"
	"image"
	"testing"

	"github.com/yuin/goldmark/text"
)

// 缩进的所有权回归：列表项里的折行、行内代码药丸的 padding、嵌套列表再右移
// 一层，都只能作用于项内的局部游标。块级 l.x 一旦被写回，后面的兄弟项就会
// 整体平移——症状是"某个项折了行，它后面的兄弟项缩进就不对了"。
//
// 断言方式是几何量：布局期记下每个项的起始 x、标记的绘制起点与首行 y，
// 兄弟项之间必须完全相等。不比对基准图。

// nestedWrapSample 是缺陷的最小复现：嵌套列表第一项折行且以行内代码开头，
// 第二项是纯文本，两者标记必须落在同一列。
const nestedWrapSample = "- 父项一\n" +
	"- 昨日新增两处配置漂移，已修复一处\n" +
	"  - `/etc/qqbot/config.toml` 的 `[http].timeout` 被手工改过，需要复核一遍配置文件与运行参数是否一致\n" +
	"  - 子项二纯文本\n" +
	"- 父项二\n"

// orderedWrapSampleOrdered 与上面同构，只是嵌套层换成了有序列表：标记宽度由
// 右对齐消掉，正文起点不该随序号变化。
const orderedWrapSample = "1. 停掉旧进程，把上一个可执行文件保留为 `qqbot.bak`\n" +
	"   1. `/etc/qqbot/config.toml` 的 `[http].timeout` 被手工改过，需要复核一遍配置文件与运行参数是否一致\n" +
	"   2. 子项二纯文本\n" +
	"2. 启动新进程并等待健康检查通过\n"

// quoteListWrapSample 是块引用里的列表：引用整体右移一个固定量，项内的
// 折行不能把这个位移累加进去。
const quoteListWrapSample = "> 维护窗口的注意事项写在下面，这段引用里既有正文也有列表。\n" +
	">\n" +
	"> - `/etc/qqbot/config.toml` 的 `[http].timeout` 被手工改过，需要复核一遍配置文件与运行参数是否一致\n" +
	"> - 子项二纯文本\n"

// traceListItems 跑一遍布局，收集每个列表项的块级几何。
func traceListItems(t *testing.T, src string) []listItemGeom {
	t.Helper()

	l := newLayout(testRenderer(t).faces.scope(), src)
	var items []listItemGeom
	l.items = &items

	doc := markdown.Parser().Parse(text.NewReader([]byte(src)))
	if err := l.blocks(context.Background(), doc); err != nil {
		t.Fatalf("blocks: %v", err)
	}
	return items
}

func itemsAtDepth(items []listItemGeom, depth int) []listItemGeom {
	var out []listItemGeom
	for _, it := range items {
		if it.depth == depth {
			out = append(out, it)
		}
	}
	return out
}

// requireWrapped 校验用例前提：blocks[0] 这一项真的折了行，否则这条用例
// 测不到"折行之后的兄弟项"。
func requireWrapped(t *testing.T, items []listItemGeom) {
	t.Helper()
	if len(items) < 2 {
		t.Fatalf("同一层的列表项只有 %d 个，构不成兄弟项", len(items))
	}
	if gap := items[1].y - items[0].y; gap < 2*lhBody {
		t.Fatalf("前提不成立：第一项没有折行（项首行间距 %.0f < %.0f）", gap, 2*lhBody)
	}
}

// requireSameColumn 断言同一层所有兄弟项的起始 x 与标记起点完全相等。
func requireSameColumn(t *testing.T, items []listItemGeom) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		if got, want := items[i].x, items[0].x; got != want {
			t.Errorf("第 %d 项的起始 x = %v，期望 %v（与第一项相同）", i+1, got, want)
		}
		if got, want := items[i].markerX, items[0].markerX; got != want {
			t.Errorf("第 %d 项的标记 x = %v，期望 %v（与第一项相同）", i+1, got, want)
		}
	}
}

// 无序嵌套列表：第一项折行且以行内代码开头，两个兄弟项的标记必须对齐。
func TestNestedListSiblingsShareMarkerColumn(t *testing.T) {
	items := traceListItems(t, nestedWrapSample)

	outer := itemsAtDepth(items, 1)
	if len(outer) != 3 {
		t.Fatalf("深度 1 的列表项 = %d，期望 3", len(outer))
	}
	requireSameColumn(t, outer)

	nested := itemsAtDepth(items, 2)
	requireWrapped(t, nested)
	requireSameColumn(t, nested)

	// 缩进阶梯：每深一层只右移一个 listIndent，不多不少。
	if got, want := nested[0].x-outer[0].x, listIndent; got != want {
		t.Errorf("嵌套层相对父层的缩进 = %v，期望 %v", got, want)
	}
	if got, want := outer[0].x, padX; got != want {
		t.Errorf("顶层项起始 x = %v，期望 %v", got, want)
	}
}

// 有序列表同构用例：序号标记右对齐，正文起点不能因为前一序号更宽而漂移。
func TestOrderedListSiblingsShareMarkerColumn(t *testing.T) {
	items := traceListItems(t, orderedWrapSample)

	outer := itemsAtDepth(items, 1)
	requireSameColumn(t, outer)

	nested := itemsAtDepth(items, 2)
	requireWrapped(t, nested)
	requireSameColumn(t, nested)
}

// 块引用内的列表：引用自己右移 quoteBar+quotePad，列表项再右移 listIndent；
// 项内折行不得把这个位移写回块级游标。
func TestBlockquoteListSiblingsShareMarkerColumn(t *testing.T) {
	items := traceListItems(t, quoteListWrapSample)

	quoted := itemsAtDepth(items, 1)
	requireWrapped(t, quoted)
	requireSameColumn(t, quoted)

	if got, want := quoted[0].x, padX+quoteBar+quotePad; got != want {
		t.Errorf("引用内列表项起始 x = %v，期望 %v", got, want)
	}
}

// 项内放表格、代码块、引用这些"自己也要吃宽度"的块时，块级游标同样只能
// 归还给项自身：它们之后的兄弟项必须还在同一列上。
func TestListSiblingsAfterHeavyChildrenShareMarkerColumn(t *testing.T) {
	const src = "- 项内表格：\n" +
		"\n" +
		"  | 服务 | 端口 |\n" +
		"  | --- | --- |\n" +
		"  | `qqbot` | 8080 |\n" +
		"- 项内代码块：\n" +
		"\n" +
		"  ```go\n" +
		"  l.x += listIndent\n" +
		"  ```\n" +
		"- 项内引用：\n" +
		"\n" +
		"  > 引用正文 `code`\n" +
		"- 普通兄弟项\n"

	items := traceListItems(t, src)
	outer := itemsAtDepth(items, 1)
	if len(outer) != 4 {
		t.Fatalf("深度 1 的列表项 = %d，期望 4", len(outer))
	}
	requireSameColumn(t, outer)
}

// 布局期的起始 x 最终落到像素上：两个兄弟项的标记必须占同一列。取每项首行
// 行盒内最左的深色像素——列表标记就是这一行最左的墨迹。
func TestNestedListMarkersLandOnSameColumn(t *testing.T) {
	src := nestedWrapSample
	items := traceListItems(t, src)
	nested := itemsAtDepth(items, 2)
	requireWrapped(t, nested)

	img := decodePNG(t, renderPNG(t, src))

	col := func(it listItemGeom) int {
		return leftmostInk(img, it.y, it.y+lhBody)
	}
	if got, want := col(nested[1]), col(nested[0]); got != want {
		t.Errorf("第二个兄弟项的标记列 = %d，期望 %d（与第一个相同）", got, want)
	}
}

// leftmostInk 返回 [y0,y1) 行内最左的深色像素 x，没有墨迹时返回 -1。
// 阈值取 0xd0：行内代码药丸的底色是近白的 #f0f1f2，不算墨迹。
func leftmostInk(m image.Image, y0, y1 float64) int {
	b := m.Bounds()
	best := -1
	for y := int(y0); y < int(y1) && y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bb, _ := m.At(x, y).RGBA()
			if r>>8 < 0xd0 || g>>8 < 0xd0 || bb>>8 < 0xd0 {
				if best < 0 || x < best {
					best = x
				}
				break
			}
		}
	}
	return best
}

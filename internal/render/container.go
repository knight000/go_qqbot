package render

import (
	"fmt"
	"image/color"
	"math"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
)

// ---- 列表 ----

// listItemGeom 是一个列表项的块级几何，只用于测试断言缩进不漂移。
//
// x 是项起始（标记列的左基准），markerX 是标记实际的绘制起点，y 是项首行
// 的行盒顶端。兄弟项之间 x 与 markerX 必须完全相等：任何"把行内推进量写回
// 块级游标"的错误都会让后面的项整体平移。
type listItemGeom struct {
	depth   int
	x       float64
	markerX float64
	// markerRight 是标记的右缘。同一列表里无序符号、序号、任务复选框共用
	// 这一条边（宽度不同，左缘必然不同），混排时标记才不会一跳一跳。
	markerRight float64
	y           float64
}

// list 画有序/无序列表。标记单独占一列，内容整体右移 listIndent：这样嵌套
// 列表只要再右移一次就自然对齐，不需要为每层算一套位置。
func (l *layout) list(n *ast.List) error {
	// 嵌套列表的 margin-top 是 0（CSS 里 ul ul 没有上边距），否则每深一层
	// 就会多出一整段空白。
	top := mbPara
	if l.nested > 0 {
		top = 0
	}
	l.gap(top)

	idx := n.Start
	if idx <= 0 {
		idx = 1
	}

	l.nested++
	defer func() { l.nested-- }()

	for item := n.FirstChild(); item != nil; item = item.NextSibling() {
		li, ok := item.(*ast.ListItem)
		if !ok {
			continue
		}
		marker := "•"
		if n.IsOrdered() {
			marker = fmt.Sprintf("%d.", idx)
			idx++
		}
		task, checked := itemTask(li)
		if err := l.listItem(li, marker, task, checked); err != nil {
			return err
		}
	}
	l.bottom(mbPara)
	return nil
}

// listItem 画一个列表项。
//
// 缩进的状态边界在这里：进入时记下项起始的块级游标，退出时**无条件**恢复
// （用 defer，错误提前返回也恢复）。项内的所有推进——标记列、续行悬挂缩进、
// 嵌套列表再右移一层——都以这个起始值为基准，谁都不许改动块级 x 的"所有权"。
func (l *layout) listItem(li *ast.ListItem, marker string, task, checked bool) error {
	l.gap(0) // 项间用 liGap，靠 margin collapsing 取较大值
	markerX, markerW := l.drawMarker(marker, task, checked)
	if l.items != nil {
		*l.items = append(*l.items, listItemGeom{
			depth: l.nested, x: l.x, markerX: markerX,
			markerRight: markerX + markerW, y: l.y,
		})
	}

	startX, startRight := l.x, l.right
	defer func() { l.x, l.right = startX, startRight }()

	l.x += listIndent

	for c := li.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Paragraph:
			if err := l.textBlock(collectSpans(v, l.src), WeightRegular, fsBody, lhBody); err != nil {
				return err
			}
		case *ast.TextBlock:
			if err := l.textBlock(collectSpans(v, l.src), WeightRegular, fsBody, lhBody); err != nil {
				return err
			}
		default:
			if err := l.block(c); err != nil {
				return err
			}
		}
	}

	l.bottom(liGap)
	return nil
}

// drawMarker 在列表内容列之前画标记，返回标记的绘制起点与宽度。所有标记
// 右对齐到同一条边：有序列表的序号位数变化、无序符号与任务复选框混排时，
// 正文起点都不动。
func (l *layout) drawMarker(marker string, task, checked bool) (x, w float64) {
	mf, err := l.faces.face(FontSans, WeightRegular, fsBody)
	if err != nil {
		return l.x, 0
	}
	baseline := baselineOf(mf, l.y, lhBody)

	// 所有标记共用一条右缘：无序符号、序号、复选框都右对齐到标记列末端。
	// 之前复选框固定从 listCheckX 起画，混排时它会比项目符号左移 26px，
	// 同一列表里标记一跳一跳。
	markerRight := l.x + listIndent - listMarkerGap

	if task {
		x = markerRight - checkSize
		l.checkbox(x, l.y+(lhBody-checkSize)/2, checked)
		return x, checkSize
	}

	w = measureString(mf, marker)
	col := colFg
	if l.muted {
		col = colMuted
	}
	x = markerRight - w
	l.text(marker, mf, col, x, baseline)
	return x, w
}

// itemTask 判断列表项是否是任务项：GFM 把复选框插在第一个段落的最前面。
func itemTask(li *ast.ListItem) (task, checked bool) {
	first := li.FirstChild()
	if first == nil {
		return false, false
	}
	for c := first.FirstChild(); c != nil; c = c.NextSibling() {
		if cb, ok := c.(*east.TaskCheckBox); ok {
			return true, cb.IsChecked
		}
	}
	return false, false
}

func (l *layout) checkbox(x, y float64, checked bool) {
	if checked {
		l.roundRect(x, y, checkSize, checkSize, 6, colAccent)
		// 白色对勾：两段粗线，避免为一个标记引入图标路径。
		l.line(x+checkSize*0.24, y+checkSize*0.52,
			x+checkSize*0.43, y+checkSize*0.72, 3, color.White)
		l.line(x+checkSize*0.43, y+checkSize*0.72,
			x+checkSize*0.78, y+checkSize*0.3, 3, color.White)
		return
	}
	// 未勾选：白底 + 灰框。
	l.fillRect(x, y, checkSize, checkSize, color.White)
	l.strokeRect(x, y, checkSize, checkSize, borderW, colMuted)
}

// line 用逐点 stamp 画一条粗线。只有复选框的对勾与表格线用得上，不值得
// 为它引入一个抗锯齿线段栅格器。
func (l *layout) line(x0, y0, x1, y1, w float64, c color.Color) {
	steps := int(math.Max(math.Abs(x1-x0), math.Abs(y1-y0)))
	if steps <= 0 {
		l.fillRect(x0-w/2, y0-w/2, w, w, c)
		return
	}
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x := x0 + (x1-x0)*t
		y := y0 + (y1-y0)*t
		l.fillRect(x-w/2, y-w/2, w, w, c)
	}
}

// ---- 块引用 ----

// blockquote 画左侧竖线。两个容易错的几何：
//
//   - border-left 在 padding 之外：文本左边界 = 竖线宽 + 左内边距。
//   - 竖线高度 = 内容盒，不含最后一个子块的 margin-bottom（CSS 的
//     blockquote > :last-child { margin-bottom: 0 }）。
func (l *layout) blockquote(n ast.Node) error {
	l.gap(mbPara)
	top := l.y

	// 与列表项同样的所有权规则：引用的缩进只属于引用自己的子块，退出时
	// 无条件还给调用方（错误路径也一样）。
	startX, startRight := l.x, l.right
	defer func() { l.x, l.right = startX, startRight }()
	l.x = startX + quoteBar + quotePad

	// 恢复而不是置 false：嵌套引用退出内层时，外层仍然应当是 muted。
	prevMuted := l.muted
	l.muted = true
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if err := l.block(c); err != nil {
			l.muted = prevMuted
			return err
		}
	}
	l.muted = prevMuted

	// 只取内容底部：子块设置的 prevBottom 不能算进竖线。
	bottom := l.y
	l.fillRect(startX, top, quoteBar, bottom-top, colBorder)

	l.bottom(mbPara)
	return nil
}

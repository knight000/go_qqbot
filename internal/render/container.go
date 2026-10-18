package render

import (
	"fmt"
	"image/color"
	"math"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
)

// ---- 列表 ----

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
		l.gap(0) // 项间用 liGap，靠 margin collapsing 取较大值

		marker := "•"
		if n.IsOrdered() {
			marker = fmt.Sprintf("%d.", idx)
			idx++
		}
		task, checked := itemTask(li)
		l.drawMarker(marker, task, checked)

		oldX, oldRight := l.x, l.right
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

		l.x, l.right = oldX, oldRight
		l.bottom(liGap)
	}
	l.bottom(mbPara)
	return nil
}

// drawMarker 在列表内容列之前画标记。有序列表右对齐，数字位数变化时正文
// 起点不动。
func (l *layout) drawMarker(marker string, task, checked bool) {
	mf, err := l.faces.face(FontSans, WeightRegular, fsBody)
	if err != nil {
		return
	}
	baseline := baselineOf(mf, l.y, lhBody)

	if task {
		l.checkbox(l.x+listCheckX, l.y+(lhBody-checkSize)/2, checked)
		return
	}

	w := measureString(mf, marker)
	col := colFg
	if l.muted {
		col = colMuted
	}
	l.text(marker, mf, col, l.x+listIndent-listMarkerGap-w, baseline)
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

	oldX, oldRight := l.x, l.right
	l.x += quoteBar + quotePad

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
	l.x, l.right = oldX, oldRight
	l.fillRect(l.x, top, quoteBar, bottom-top, colBorder)

	l.bottom(mbPara)
	return nil
}

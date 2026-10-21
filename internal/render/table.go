package render

import (
	"image/color"
	"math"

	east "github.com/yuin/goldmark/extension/ast"
)

// table 画 GFM 表格。
//
// 两趟列宽：先量每列的 max-content（与最长不可断片段），再三段式分配：
// 放得下就用 max-content、放不下按可收缩余量等比压缩、压到可读性下限后
// 仍超宽就让超长 token 在单元格内硬断行。
//
// 保留 GitHub 的 max-content 收缩：表格不必满宽，列宽由内容决定。
func (l *layout) table(t *east.Table) error {
	l.gap(mbPara)

	head, body, ncol := tableCells(t, l.src)
	if ncol == 0 {
		return nil
	}

	tl, err := l.layoutTable(head, body, ncol)
	if err != nil {
		return err
	}
	widths, padH, total := tl.widths, tl.padH, tl.total

	rowHeight := func(cells [][]span, bold bool) (float64, error) {
		w := WeightRegular
		if bold {
			w = WeightBold
		}
		lines := 1
		for i, sp := range cells {
			if i >= ncol {
				break
			}
			n, err := l.cellLines(sp, widths[i], w)
			if err != nil {
				return 0, err
			}
			if n > lines {
				lines = n
			}
		}
		return float64(lines)*lhBody + 2*tablePadV, nil
	}

	drawRow := func(cells [][]span, bold bool, bg color.Color) (float64, error) {
		w := WeightRegular
		if bold {
			w = WeightBold
		}
		h, err := rowHeight(cells, bold)
		if err != nil {
			return 0, err
		}
		if bg != nil {
			l.fillRect(l.x, l.y, total, h, bg)
		}
		cx := l.x
		for i := 0; i < ncol; i++ {
			if i < len(cells) {
				gs, err := l.glyphs(cells[i], w, fsBody)
				if err != nil {
					return 0, err
				}
				lines := l.wrap(gs, widths[i])
				// 单元格默认 vertical-align: middle，单行内容与多行内容
				// 垂直居中对齐，而不是一律贴顶。
				y := l.y + (h-float64(len(lines))*lhBody)/2
				fallback, err := l.faces.face(FontSans, w, fsBody)
				if err != nil {
					return 0, err
				}
				for _, ln := range lines {
					l.drawLine(ln, cx+padH, y, lhBody, fallback)
					y += lhBody
				}
			}
			cx += widths[i] + 2*padH
		}
		l.y += h
		return h, nil
	}

	top := l.y
	headBottom := top
	if len(head) > 0 {
		if _, err := drawRow(head, true, nil); err != nil {
			return err
		}
		headBottom = l.y
	}
	for i, row := range body {
		var bg color.Color
		if i%2 == 1 {
			bg = colMutedBg
		}
		if _, err := drawRow(row, false, bg); err != nil {
			return err
		}
	}
	bottom := l.y

	// 网格线最后画，盖在背景与文字之间的空隙上。
	// th 与 td 都有 border：外框、竖线之外还要单独画表头下边框，
	// 否则表头与首行之间是空的。
	l.fillRect(l.x, top, total, borderW, colBorder)
	l.fillRect(l.x, bottom-borderW, total, borderW, colBorder)
	l.fillRect(l.x, top, borderW, bottom-top, colBorder)
	l.fillRect(l.x+total-borderW, top, borderW, bottom-top, colBorder)
	cx := l.x
	for i := 0; i < ncol-1; i++ {
		cx += widths[i] + 2*padH
		l.fillRect(cx, top, borderW, bottom-top, colBorder)
	}
	if len(body) > 0 && headBottom > top {
		l.fillRect(l.x, headBottom-borderW/2, total, borderW, colBorder)
	}

	l.bottom(mbPara)
	return nil
}

// tableCells 把表格拍平成表头与各数据行的单元格。列数取表头行与数据行
// 的最大值：某些解析器（与手写 markdown）允许行的单元格数不一致。
func tableCells(t *east.Table, src []byte) (head [][]span, body [][][]span, ncol int) {
	for n := t.FirstChild(); n != nil; n = n.NextSibling() {
		switch row := n.(type) {
		case *east.TableHeader:
			for c := row.FirstChild(); c != nil; c = c.NextSibling() {
				if tc, ok := c.(*east.TableCell); ok {
					head = append(head, collectSpans(tc, src))
				}
			}
		case *east.TableRow:
			var cells [][]span
			for c := row.FirstChild(); c != nil; c = c.NextSibling() {
				if tc, ok := c.(*east.TableCell); ok {
					cells = append(cells, collectSpans(tc, src))
				}
			}
			body = append(body, cells)
		}
	}
	ncol = len(head)
	for _, row := range body {
		if len(row) > ncol {
			ncol = len(row)
		}
	}
	return head, body, ncol
}

// tableLayout 是列宽分配的结果：每列的内容宽度、单元格左右内边距、表格总宽。
type tableLayout struct {
	widths []float64
	padH   float64
	total  float64
}

// layoutTable 量内容并分配列宽。
//
// 单独成方法是为了让测试能对真实表格断言每列宽度，而不必在测试里重写
// 一遍测量与压缩逻辑。
func (l *layout) layoutTable(head [][]span, body [][][]span, ncol int) (tableLayout, error) {
	// 第一趟：每列的 max-content 与 min-content 宽度。表头按粗体量，
	// 否则加粗后会比列宽。min-content 是"最长的不可断片段"，用来当
	// "不破词"的下限：列宽不该窄到把 webdav 从中间劈开。
	widths := make([]float64, ncol)
	mins := make([]float64, ncol)
	measure := func(cells [][]span, bold bool) error {
		w := WeightRegular
		if bold {
			w = WeightBold
		}
		for i, sp := range cells {
			if i >= ncol {
				break
			}
			gs, err := l.glyphs(sp, w, fsBody)
			if err != nil {
				return err
			}
			if cw := glyphsWidth(gs); cw > widths[i] {
				widths[i] = cw
			}
			if mw := minContentWidth(gs); mw > mins[i] {
				mins[i] = mw
			}
		}
		return nil
	}
	if err := measure(head, true); err != nil {
		return tableLayout{}, err
	}
	for _, row := range body {
		if err := measure(row, false); err != nil {
			return tableLayout{}, err
		}
	}

	avail := l.right - l.x
	// 列数极多时，左右内边距本身就会超过画布宽度：按比例收窄内边距，
	// 不留硬下限。旧的 4px 下限与 1px 列宽下限相加是 9px/列，113 列就已经
	// 越过 1016px 的可用宽度——溢出的右边框会被画布裁掉，而读者看不出
	// 后面还有列。宁可挤，也不让内容静默消失。
	padH := tablePadH
	if need := float64(ncol) * 2 * padH; need > avail*0.4 {
		padH = avail * 0.4 / float64(ncol*2)
	}

	widths = fitColumns(widths, mins, avail, padH)
	return tableLayout{widths: widths, padH: padH, total: tableWidth(widths, padH)}, nil
}

// glyphsWidth 是字形序列的总推进量（含行内代码药丸的 padding）。
func glyphsWidth(gs []glyph) float64 {
	var width float64
	for _, g := range gs {
		width += g.adv
	}
	return width
}

// minContentWidth 是"最长的不可断片段"的宽度。压缩列宽时列不能窄于它，
// 否则 webdav 这样的单词会被从中间劈成两行——浏览器不会这么排。
func minContentWidth(gs []glyph) float64 {
	longest, cur := 0.0, 0.0
	for i, g := range gs {
		if g.br {
			longest = math.Max(longest, cur)
			cur = 0
			continue
		}
		cur += g.adv
		if i+1 < len(gs) && !gs[i+1].br && canBreak(g.r, gs[i+1].r) {
			longest = math.Max(longest, cur)
			cur = 0
		}
	}
	return math.Max(longest, cur)
}

// cellLines 是单元格折行后的行数。
func (l *layout) cellLines(sp []span, width float64, w Weight) (int, error) {
	gs, err := l.glyphs(sp, w, fsBody)
	if err != nil {
		return 0, err
	}
	return len(l.wrap(gs, width)), nil
}

func tableWidth(widths []float64, padH float64) float64 {
	var w float64
	for _, x := range widths {
		w += x + 2*padH
	}
	return w
}

// fitColumns 把 max-content 列宽压进可用宽度，三段式：
//
//  1. 总宽放得下 → 原样返回。表格不必满宽，这是 GitHub 的 max-content 行为。
//  2. 放不下 → 水位法压缩：窄列保留自己的 max-content，最宽的那几列平摊
//     被砍掉的宽度，且谁都不得窄于自己的下限。直接按宽度等比压缩会先把
//     「运行中」这种三字列压到折行，而旁边的长文本列本来还有大量富余——
//     那不是浏览器的排法。
//  3. 全部压到下限后仍超宽（列太多或画布太窄）→ 下限本身等比再收一档
//     （允许 <1px），剩下的交给单元格内硬断行。宁可把超长 token 从中间
//     断开、宁可列窄到读不清，也不能让表格溢出画布——溢出的部分会被画布
//     裁掉，读者连"这里还有列"都看不出来。
//
// 下限分两层：tableMinColWidth 是**软**下限（可读性，且不超过均分预算）；
// "不破词"（列宽不窄于最长不可断片段）只在水位法里有余量时满足。两层都
// 写成硬下限会出事：一条 60 字符的 token 的 min-content 就是它自己，它会
// 把预算吃光，剩下的可断行列只能退到一两个字——这正是要修的缺陷。
func fitColumns(maxContent, minContent []float64, avail, padH float64) []float64 {
	ncol := len(maxContent)
	out := append([]float64{}, maxContent...)
	if ncol == 0 || tableWidth(out, padH) <= avail {
		return out
	}

	remaining := avail - float64(ncol)*2*padH

	// 均分宽度是"不破词"天花板：任何一列都不该因为内容不可断而拿到
	// 超过均分的宽度，否则其余列连可读性下限都保不住。
	share := avail / float64(ncol)
	// 可读性下限同样受均分预算约束：列数一大，"每列至少 6 个半角"本身就
	// 会把表格撑出画布（113 列 × 96px ≫ 1016px）。下限是软目标，不溢出
	// 是硬约束。
	floor := math.Min(tableMinColWidth, remaining/float64(ncol))
	if floor < 0 {
		floor = 0
	}
	base := make([]float64, ncol)
	word := make([]float64, ncol)
	baseSum, upper := 0.0, 0.0
	for i := range out {
		b := math.Min(maxContent[i], floor)
		base[i] = b
		w := b
		if minContent[i] > w {
			w = math.Min(math.Min(minContent[i], share), maxContent[i])
			if w < b {
				w = b
			}
		}
		word[i] = w
		baseSum += b
		upper = math.Max(upper, maxContent[i])
	}

	if baseSum >= remaining {
		// 连硬下限都放不下：只能力保不溢出，剩下的交给折行硬断。
		scale := remaining / baseSum
		for i := range out {
			out[i] = math.Max(base[i]*scale, 0)
		}
		return out
	}

	// 软下限按预算分摊：预算够就整份给，不够就等比分，绝不为了一个长
	// token 把其它列压到可读性下限以下。
	budget, demand := remaining-baseSum, 0.0
	for i := range word {
		demand += word[i] - base[i]
	}
	ratio := 1.0
	if demand > budget {
		ratio = budget / demand
	}
	floors := make([]float64, ncol)
	for i := range out {
		floors[i] = base[i] + (word[i]-base[i])*ratio
	}

	// 二分水位：sum(min(max_i, max(cap, floor_i))) == remaining。
	// floorSum ≤ remaining < maxSum，解一定存在。
	lo, hi := 0.0, upper
	for pass := 0; pass < 40; pass++ {
		cap := (lo + hi) / 2
		sum := 0.0
		for i := range out {
			sum += math.Max(floors[i], math.Min(maxContent[i], cap))
		}
		if sum > remaining {
			hi = cap
		} else {
			lo = cap
		}
	}
	for i := range out {
		out[i] = math.Max(floors[i], math.Min(maxContent[i], lo))
	}

	// 二分只收敛到"约等于"，几十列累加后的零点几像素也会让右边框落到
	// 画布外。最后再按总和归一一次，保证 total ≤ avail 是硬约束。
	sum := 0.0
	for _, w := range out {
		sum += w
	}
	if sum > remaining {
		scale := remaining / sum
		for i := range out {
			out[i] *= scale
		}
	}
	return out
}

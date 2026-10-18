package render

import (
	"image/color"
	"math"

	east "github.com/yuin/goldmark/extension/ast"
)

// table 画 GFM 表格。
//
// 两趟列宽：
//  1. 量每列的 max-content 宽度；
//  2. 总宽超过可用宽度时按比例压缩，再在单元格内折行。
//
// 保留 GitHub 的 max-content 收缩：表格不必满宽，列宽由内容决定。
func (l *layout) table(t *east.Table) error {
	l.gap(mbPara)

	var head [][]span
	var body [][][]span
	for n := t.FirstChild(); n != nil; n = n.NextSibling() {
		switch row := n.(type) {
		case *east.TableHeader:
			for c := row.FirstChild(); c != nil; c = c.NextSibling() {
				if tc, ok := c.(*east.TableCell); ok {
					head = append(head, collectSpans(tc, l.src))
				}
			}
		case *east.TableRow:
			var cells [][]span
			for c := row.FirstChild(); c != nil; c = c.NextSibling() {
				if tc, ok := c.(*east.TableCell); ok {
					cells = append(cells, collectSpans(tc, l.src))
				}
			}
			body = append(body, cells)
		}
	}

	ncol := len(head)
	for _, row := range body {
		if len(row) > ncol {
			ncol = len(row)
		}
	}
	if ncol == 0 {
		return nil
	}

	// 第一趟：每列的 max-content 与 min-content 宽度。表头按粗体量，
	// 否则加粗后会比列宽。min-content 是"最长的不可断片段"——没有它，
	// 压缩列宽会把 webdav 这种单词从中间劈开。
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
		return err
	}
	for _, row := range body {
		if err := measure(row, false); err != nil {
			return err
		}
	}

	avail := l.right - l.x
	// 列数极多时，左右内边距本身就会超过画布宽度：按比例收窄内边距，
	// 宁可挤一点也不让表格溢出。
	padH := tablePadH
	if need := float64(ncol) * 2 * padH; need > avail*0.4 {
		padH = math.Max(4, avail*0.4/float64(ncol*2))
	}

	widths = fitColumns(widths, mins, avail, padH)

	total := tableWidth(widths, padH)
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

// fitColumns 把 max-content 列宽压进可用宽度。
//
// 按"上限从宽往窄收"的水位法分配：窄列保留自己的 max-content，最宽的那几
// 列平摊被砍掉的宽度。直接按比例压缩会把「运行中」这种三字列也压到折行，
// 而旁边的长文本列本来就有大量富余可以吸收——那不是浏览器的排法。
//
// 每列另有 min-content 地板（最长不可断片段），避免 webdav 被从中间劈开；
// 连 min-content 都放不下时按比例继续压：宁可破词，也不溢出画布。
func fitColumns(maxContent, minContent []float64, avail, padH float64) []float64 {
	ncol := len(maxContent)
	out := append([]float64{}, maxContent...)

	gaps := float64(ncol) * 2 * padH
	remaining := avail - gaps
	if tableWidth(out, padH) <= avail {
		return out
	}
	if remaining < 1 {
		remaining = 1
	}

	mins := make([]float64, ncol)
	maxSum, minSum := 0.0, 0.0
	upper := 0.0
	for i := range out {
		mins[i] = math.Min(minContent[i], maxContent[i])
		if mins[i] < 1 {
			mins[i] = 1
		}
		maxSum += maxContent[i]
		minSum += mins[i]
		upper = math.Max(upper, maxContent[i])
	}
	if minSum >= remaining {
		scale := remaining / minSum
		for i := range out {
			out[i] = math.Max(mins[i]*scale, 1)
		}
		return out
	}
	if maxSum <= remaining {
		return out
	}

	// 二分水位：sum(min(max_i, max(cap, min_i))) == remaining。
	lo, hi := 0.0, upper
	for pass := 0; pass < 40; pass++ {
		cap := (lo + hi) / 2
		sum := 0.0
		for i := range out {
			w := math.Min(maxContent[i], cap)
			if w < mins[i] {
				w = mins[i]
			}
			sum += w
		}
		if sum > remaining {
			hi = cap
		} else {
			lo = cap
		}
	}
	for i := range out {
		w := math.Min(maxContent[i], lo)
		if w < mins[i] {
			w = mins[i]
		}
		out[i] = w
	}
	return out
}

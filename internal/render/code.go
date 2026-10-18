package render

import (
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// codeBlock 画围栏或缩进代码块：等宽字体、浅灰圆角底、整块满宽。
//
// 超宽行折行而不是截断或缩字号：截断等于悄悄丢内容，缩字号会让同一张图里
// 出现两套字号阶梯。续行与代码起点对齐（悬挂缩进），读者仍能看出这是一条
// 命令的延续。
func (l *layout) codeBlock(n ast.Node) error {
	l.gap(mbCode)

	inner := l.right - l.x - 2*codePad
	var wrapped [][]glyph
	for _, line := range codeLines(n, l.src) {
		gs, err := l.glyphs([]span{{text: line, mono: true}}, WeightRegular, fsCode)
		if err != nil {
			return err
		}
		wrapped = append(wrapped, l.wrapMode(gs, inner, true)...)
	}
	if len(wrapped) == 0 {
		wrapped = [][]glyph{{}}
	}

	h := float64(len(wrapped))*lhCode + 2*codePad
	l.roundRect(l.x, l.y, l.right-l.x, h, codeRadius, colMutedBg)

	mono, err := l.faces.face(FontMono, WeightRegular, fsCode)
	if err != nil {
		return err
	}

	y := l.y + codePad
	for _, ln := range wrapped {
		l.drawLine(ln, l.x+codePad, y, lhCode, mono)
		y += lhCode
	}
	l.y += h
	l.bottom(mbCode)
	return nil
}

// codeLines 取代码块的源码行。tab 展开成四个空格：字体里没有 tab 字形，
// 不展开会变成缺字方框。
func codeLines(n ast.Node, src []byte) []string {
	var segs *text.Segments
	switch v := n.(type) {
	case *ast.FencedCodeBlock:
		segs = v.Lines()
	case *ast.CodeBlock:
		segs = v.Lines()
	}
	if segs == nil {
		return nil
	}
	out := make([]string, 0, segs.Len())
	for i := 0; i < segs.Len(); i++ {
		seg := segs.At(i)
		line := strings.TrimRight(string(seg.Value(src)), "\r\n")
		out = append(out, strings.ReplaceAll(line, "\t", "    "))
	}
	return out
}

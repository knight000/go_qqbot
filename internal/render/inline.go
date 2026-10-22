package render

import (
	"image/color"
	"math"
	"strings"
	"unicode"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
	"golang.org/x/image/font"
)

// ---- 行内 span ----

// span 是一段同源的纯文本加上继承下来的行内样式。
//
// 斜体没有样式位：v1 只内嵌 Regular/Bold 两份静态实例，斜体按常规字重
// 渲染——这是 issue #1 明确接受的偏差，不是遗漏。
type span struct {
	text   string
	bold   bool
	code   bool
	mono   bool
	link   bool
	strike bool
	small  bool
	br     bool
}

// collectSpans 把行内节点拍平成 span 序列。样式沿树向下继承：标题里的
// 行内代码仍是粗体，链接里的加粗仍是蓝色。
func collectSpans(n ast.Node, src []byte) []span {
	return collectInline(n, src, span{})
}

func collectInline(n ast.Node, src []byte, inherit span) []span {
	var out []span
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Text:
			s := inherit
			s.text = string(v.Value(src))
			out = append(out, s)
			// 源码换行 = 视觉换行：软换行与硬换行产同样的 br。
			//
			// HTML 把软换行渲染成空格，是因为浏览器要留一个折行点；我们的
			// 产物是图不是 HTML，而 canBreak 在 CJK 边界、空格之后、连字符
			// 之后都能断，不需要那个空格兼作断点。按空格画反而会把用户
			// 明确写下的换行吃掉（#32）：五行输入画成一行。
			if v.HardLineBreak() || v.SoftLineBreak() {
				out = append(out, span{br: true})
			}
		case *ast.String:
			s := inherit
			s.text = string(v.Value)
			out = append(out, s)
		case *ast.CodeSpan:
			// CodeSpan 没有独立文本字段，内容在子 Text 节点里；空白按
			// CommonMark 折叠成单个空格。
			s := inherit
			s.text = strings.Join(strings.Fields(plainText(v, src)), " ")
			s.code = true
			out = append(out, s)
		case *ast.Emphasis:
			s := inherit
			if v.Level >= 2 {
				s.bold = true
			}
			// Level == 1 是斜体：共用常规字重，样式位不变。
			out = append(out, collectInline(v, src, s)...)
		case *ast.Link:
			s := inherit
			s.link = true
			out = append(out, collectInline(v, src, s)...)
			// 链接渲染成「文字 (url)」：静态图里没有可点的东西，地址
			// 本身是读者需要的信息。小一号字避免抢正文的视觉重心。
			if dest := string(v.Destination); dest != "" && dest != plainText(v, src) {
				u := span{text: " (" + dest + ")", link: true, small: true}
				out = append(out, u)
			}
		case *ast.AutoLink:
			s := inherit
			s.text = string(v.Label(src))
			s.link = true
			out = append(out, s)
		case *ast.RawHTML:
			// 内嵌 HTML 没有子节点，取原文，避免"内容被吃掉"。
			if v.Segments != nil {
				for i := 0; i < v.Segments.Len(); i++ {
					seg := v.Segments.At(i)
					s := inherit
					s.text = string(seg.Value(src))
					out = append(out, s)
				}
			}
		case *east.TaskCheckBox:
			// 复选框由列表容器单独画，不能当正文文本重复输出。
		case *east.Strikethrough:
			s := inherit
			s.strike = true
			out = append(out, collectInline(v, src, s)...)
		default:
			out = append(out, collectInline(c, src, inherit)...)
		}
	}
	return out
}

func plainText(n ast.Node, src []byte) string {
	var sb strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Text:
			sb.Write(v.Value(src))
		case *ast.String:
			sb.Write(v.Value)
		case *ast.AutoLink:
			sb.Write(v.Label(src))
		default:
			sb.WriteString(plainText(c, src))
		}
	}
	return sb.String()
}

// glyph 是逐字符的绘制单元。adv 是**含行内水平 padding** 的推进量——折行
// 器只认 adv，padding 不计进来药丸就会盖住相邻字符。
type glyph struct {
	r      rune
	f      font.Face
	col    color.Color
	size   float64
	code   bool
	strike bool
	// missing 标记字体里没有这个字形：画方框，而不是画一个空格。
	missing bool
	br      bool

	adv  float64
	padL float64
	padR float64
}

// inlinePad 是行内代码的左右内边距（GitHub 的 .4em）。
func inlinePad() float64 { return math.Round(fsCode * 0.4) }

// sameRunStyle 判断两个相邻字形会不会被 drawLine 画进同一次 DrawString。
// 分组键（face、颜色、删除线、换行、缺字）只在这里写一遍：折行时的 kern
// （kernOf）与绘制时的分组必须用同一个判据，两处分叉会让"算出来的行宽"
// 与"画出来的行宽"差出几个 kern，表现为行尾留缝或文字越界。
func sameRunStyle(a, b glyph) bool {
	return !a.br && !b.br && !a.missing && !b.missing &&
		a.f == b.f && a.col == b.col && a.strike == b.strike
}

// kernOf 是一对相邻字形的字距调整（像素，通常为负）。跨 run 时为 0：
// 那里不会真的应用 kern，算进推进量就会多扣一截。
func kernOf(prev, cur glyph) float64 {
	if !sameRunStyle(prev, cur) {
		return 0
	}
	return float64(prev.f.Kern(prev.r, cur.r)) / 64
}

// pxAdvance 是第 i 个字形相对前一个的推进量：自身 adv 加与同行前一个字形
// 的 kern。i 为 0（行首或 run 首）时没有前一个可加。
//
// 折行判决、绘制推进、列宽测量都必须走这一个函数：font.Drawer 测量与绘制
// 都含 kern，只累加 GlyphAdvance 会让长行提前几个像素断、run 边界留缝。
func pxAdvance(gs []glyph, i int) float64 {
	if i <= 0 {
		return gs[i].adv
	}
	return gs[i].adv + kernOf(gs[i-1], gs[i])
}

// glyphs 把 span 拍平成逐字符的字形。首选字族按字符选：CJK 走 FontSC，
// 拉丁走 FontSans；行内代码用 FontMono。首选族没有该字形时沿回退链继续问，
// 三族都没有才算缺字（见 pickFace）。
func (l *layout) glyphs(sp []span, w Weight, size float64) ([]glyph, error) {
	var out []glyph
	for _, s := range sp {
		if s.br {
			out = append(out, glyph{br: true})
			continue
		}
		sz := size
		if s.code || s.mono {
			sz = fsCode
		} else if s.small {
			sz = fsSmall
		}
		weight := w
		if s.bold {
			weight = WeightBold
		}

		// 颜色优先级：链接的强调色压过引用块的 muted。反过来会让引用块里的
		// 链接与正文同色，静态图里唯一的链接线索就没了。
		col := colFg
		if l.muted {
			col = colMuted
		}
		if s.link {
			col = colAccent
		}

		start := len(out)
		for _, r := range expandInline(s.text) {
			pref := familyOfRune(r)
			if (s.code || s.mono) && !isCJK(r) {
				pref = FontMono
			}
			f, width, ok, err := l.pickFace(r, pref, weight, sz)
			if err != nil {
				return nil, err
			}
			missing := !ok
			if missing {
				// 三族都没有这个字形：给个半角宽度的方框，别让整行挤成一团。
				width = sz * 0.5
				l.noteMissing(r)
			}
			out = append(out, glyph{
				r: r, f: f, col: col, size: sz,
				code: s.code, strike: s.strike,
				missing: missing, adv: width,
			})
		}
		if s.code && len(out) > start {
			// 药丸的水平 padding 必须计入推进量，否则会盖住前一个字、
			// 后一个字贴边。两端各一半，折行时也不会丢。
			pad := inlinePad()
			out[start].padL = pad
			out[start].adv += pad
			out[len(out)-1].padR = pad
			out[len(out)-1].adv += pad
		}
	}
	return out, nil
}

// expandInline 是行内文本的**唯一**预处理点：tab 展开成 4 个空格（与代码块
// 的展开一致）、控制字符丢弃。
//
// 字体里没有 tab 与多数 C0/C1 控制字符的字形：不预处理就会被当成缺字画成
// 方框，还会让"字体子集外"的 warn 误报——那是一条会把真正缺字淹掉的假警报。
// 代码块路径早在 codeLines 里展开过 tab，行内是漏掉的另一半。控制字符连
// 推进量都不占：它们没有可画的形态，留白只会让行距看着莫名变大。
func expandInline(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r == '\t' || unicode.IsControl(r) }) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			sb.WriteString("    ")
		case unicode.IsControl(r):
			// 丢弃。
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// pickFace 按回退链接逐族取字形，返回第一个**真有该字形**的 face 及其推进量。
//
// 三族覆盖是互补的：Noto Sans 没有 → ✓ ★ ● 这类符号，Noto Sans SC 有；等宽
// 族有框线却未必有 ✓。只按码位选唯一一族会把子集内本来就有的字符画成方框，
// 所以每次都要问 face 本身（GlyphAdvance 的 ok 就是 cmap 的答案）。
//
// 三族都没有时返回首选族的 face 与 ok=false，调用方据此画方框——face 仍要
// 有值，药丸与方框的度量都靠它。
func (l *layout) pickFace(r rune, pref Family, w Weight, size float64) (font.Face, float64, bool, error) {
	var first font.Face
	for i, fam := range faceOrder(pref) {
		f, err := l.faces.face(fam, w, size)
		if err != nil {
			return nil, 0, false, err
		}
		if i == 0 {
			first = f
		}
		if adv, ok := f.GlyphAdvance(r); ok {
			return f, float64(adv) / 64, true, nil
		}
	}
	return first, 0, false, nil
}

// faceOrder 是回退链：首选族 → 另一个正文族 → 等宽族。首选族放最前是为了
// 让覆盖重叠的字符（ASCII、全角标点）留在本来该用的字体里，视觉不跳。
func faceOrder(pref Family) []Family {
	switch pref {
	case FontSC:
		return []Family{FontSC, FontSans, FontMono}
	case FontMono:
		return []Family{FontMono, FontSC, FontSans}
	default:
		return []Family{FontSans, FontSC, FontMono}
	}
}

// familyOfRune 是"逐字符选首选字族"的唯一决策点；真正的字形可能来自回退链
// 上的下一族，见 pickFace。
func familyOfRune(r rune) Family {
	if isCJK(r) {
		return FontSC
	}
	return FontSans
}

// isCJK 覆盖汉字、假名、谚文与全角标点（CJK 标点区、全角区）。
func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
		(r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

// ---- 行绘制 ----

// drawLine 按**块的字面**（fallback）的 Metrics 定位基线，而不是把文字
// 垂直居中，也不用行内第一个字形的 face：CSS 的行盒以 strut 为准，行内
// 代码、小一号的链接地址都应当坐在同一基线上，不能因为行首是个药丸就把
// 整行文字抬高一两像素。
func (l *layout) drawLine(gs []glyph, x, y, lineH float64, fallback font.Face) {
	baseline := baselineOf(fallback, y, lineH)

	// 药丸先铺底：同一行的药丸互不重叠，分两趟比边画边垫简单。
	l.drawCodePills(gs, x, baseline)

	cx := x
	for i := 0; i < len(gs); {
		if gs[i].missing {
			j := i
			for j < len(gs) && gs[j].missing {
				l.drawTofu(cx, baseline, gs[j])
				cx += gs[j].adv
				j++
			}
			i = j
			continue
		}

		j := i
		var sb strings.Builder
		width := 0.0
		for j < len(gs) && sameRunStyle(gs[i], gs[j]) {
			sb.WriteRune(gs[j].r)
			// 与 drawLine 的分组条件一致：run 内的相邻字形才有 kern。
			width += pxAdvance(gs, j)
			j++
		}
		// 药丸左 padding 已经算在首个字形的 adv 里，文本从 padding 之后开始。
		l.text(sb.String(), gs[i].f, gs[i].col, cx+gs[i].padL, baseline)
		// 推进量以 adv 之和为准：text 的测量宽度不含 padding。
		startX := cx
		cx += width

		if gs[i].strike {
			l.strikeThrough(startX+gs[i].padL, cx-gs[j-1].padR, baseline, gs[i].size)
		}
		i = j
	}
}

func (l *layout) drawCodePills(gs []glyph, x, baseline float64) {
	px := x
	for i := 0; i < len(gs); {
		if !gs[i].code {
			px += gs[i].adv
			i++
			continue
		}
		j, w := i, 0.0
		for j < len(gs) && gs[j].code && !gs[j].br {
			w += pxAdvance(gs, j)
			j++
		}
		asc, desc := faceAscentDescent(gs[i].f)
		padV := math.Round(fsCode * 0.2)
		l.roundRect(px, baseline-asc-padV, w, asc+desc+2*padV, codeRadius, colNeutBg)
		px += w
		i = j
	}
}

// strikeThrough 画删除线：位置在基线上方约 0.3em，粗细 2px。
func (l *layout) strikeThrough(x0, x1, baseline, size float64) {
	l.fillRect(x0, baseline-size*0.3, x1-x0, borderW, colMuted)
}

// drawTofu 把缺字画成方框。方框比空格诚实——读者能看出"这里有个字，
// 字体里没有"，而不是以为原文就有空白。
func (l *layout) drawTofu(x, baseline float64, g glyph) {
	// 方框画成正方形并落在基线上，视觉上更像一个"占位字符"而不是细条。
	side := math.Min(g.adv*0.9, g.size*0.7)
	top := baseline - g.size*0.72
	l.strokeRect(x+(g.adv-side)/2, top, side, side, borderW, colMuted)
}

func (l *layout) strokeRect(x, y, w, h, t float64, c color.Color) {
	l.fillRect(x, y, w, t, c)
	l.fillRect(x, y+h-t, w, t, c)
	l.fillRect(x, y, t, h, c)
	l.fillRect(x+w-t, y, t, h, c)
}

func faceAscentDescent(f font.Face) (asc, desc float64) {
	m := f.Metrics()
	return float64(m.Ascent) / 64, float64(m.Descent) / 64
}

// baselineOf 把文字垂直放在行盒里：CSS 的行盒高度不等分，ascent/descent
// 的比例由字体决定，按 Metrics 算才和浏览器一致。
func baselineOf(f font.Face, y, lineH float64) float64 {
	m := f.Metrics()
	asc := float64(m.Ascent) / 64
	desc := float64(m.Descent) / 64
	return y + (lineH-(asc+desc))/2 + asc
}

func measureString(f font.Face, s string) float64 {
	d := font.Drawer{Face: f}
	return float64(d.MeasureString(s)) / 64
}

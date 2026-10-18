package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// markdown 是解析器。goldmark 的 Parser 可并发使用（每次 Parse 自带 context），
// 所以进程内只建一次，不按调用重建。
var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

// renderSlots 是**包级**信号量：内存是进程级资源，每个 Renderer 实例一份
// 等于没限——core 里每个插件都会拿到同一个 Renderer，但测试与将来可能的
// 多实例都不能绕过这个上限。
var renderSlots = make(chan struct{}, maxConcurrentRenders)

// Renderer 把受支持的 markdown 画成 PNG。
//
// 零配置：宽度、字号、主题都是视觉基线的一部分，调用方不可调。
// 并发安全：布局状态都在每次调用的 layout 里，face 按渲染独占分配（见 faceCache）。
type Renderer struct {
	set   *FontSet
	log   *slog.Logger
	faces *faceCache
}

// New 构造渲染器。字体集为 nil 时渲染请求会报错（构造不 panic：装配错误
// 应该在启动期暴露，而不是在第一次出图时把进程带走）。
//
// 字体解析不在这里做——那是 LoadFonts 的职责，启动期一次。
func New(set *FontSet, log *slog.Logger) *Renderer {
	if log == nil {
		log = slog.Default()
	}
	return &Renderer{set: set, log: log, faces: newFaceCache(set)}
}

// Markdown 把 markdown 文档源画成 PNG，结果以 host.Image 返回（内存字节，不落盘）。
//
// 两遍布局：第一遍只累计纵向位置得到总高，第二遍才真正绘制。画布高度必须
// 在第一遍结束时才知道，所以不能一边画一边扩容。
func (r *Renderer) Markdown(ctx context.Context, src string) (host.Image, error) {
	// 先看 ctx 再看槽位：已取消的调用不该排队，也不该赌 select 随机选中发送分支。
	if err := ctx.Err(); err != nil {
		return host.Image{}, renderTimeout(err)
	}
	if err := acquireRenderSlot(ctx); err != nil {
		return host.Image{}, err
	}
	defer releaseRenderSlot()

	// 拿到槽位后、解析前再确认一次：等待期间被取消的调用不应该继续烧 CPU。
	if err := ctx.Err(); err != nil {
		return host.Image{}, renderTimeout(err)
	}
	if r.set == nil {
		return host.Image{}, errors.New("render: 字体集未初始化")
	}

	start := time.Now()
	doc := markdown.Parser().Parse(text.NewReader([]byte(src)))

	// 一次渲染独占一组 face：opentype.Face 不支持并发使用，见 faceCache 注释。
	faces := r.faces.scope()
	defer faces.release()

	// 第一遍：量高度。同时校验上限、逐块检查 ctx。
	measure := newLayout(faces, src)
	if err := measure.blocks(ctx, doc); err != nil {
		return host.Image{}, err
	}
	height := int(math.Ceil(measure.y + padY))
	if height > maxHeight {
		return host.Image{}, &host.RenderTooLargeError{NeedHeight: height}
	}

	img := image.NewRGBA(image.Rect(0, 0, int(imgWidth), height))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	// 第二遍：画。y 的推进与第一遍完全一致，因为两遍走的是同一套块代码。
	canvas := newLayout(faces, src)
	canvas.img = img
	if err := canvas.blocks(ctx, doc); err != nil {
		return host.Image{}, err
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return host.Image{}, fmt.Errorf("render: PNG 编码: %w", err)
	}

	r.log.Debug("渲染完成",
		"width", int(imgWidth), "height", height,
		"bytes", buf.Len(), "cost", time.Since(start))
	return host.ImageFromBytes(buf.Bytes()), nil
}

func acquireRenderSlot(ctx context.Context) error {
	select {
	case renderSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		// 排队也是可取消的：插件 handler 只有 3 秒预算，排队时间算在里面。
		return renderTimeout(ctx.Err())
	}
}

func releaseRenderSlot() { <-renderSlots }

// renderTimeout 把 ctx 的取消包装成 host 的哨兵，插件的 errors.Is 才认得。
func renderTimeout(cause error) error {
	return fmt.Errorf("%w: %w", host.ErrRenderTimeout, cause)
}

// ---- face 缓存 ----

// sizedFaceKey 是 (族, 字重, 字号) 三元组。字号取整：视觉基线的字号阶梯
// 都是整数，取整让重复请求命中同一份 face。
type sizedFaceKey struct {
	family Family
	weight Weight
	size   int
}

// faceCache 缓存 opentype.NewFace 的结果，key 是 (族, 字重, 字号)。
//
// 关键约束：**font.Face 不能跨 goroutine 共享**。opentype.Face 自带光栅化
// 缓冲区（mask/rast），Glyph 会复写它们——两个并发渲染共用一个 face 会直接
// panic（实测 index out of range / 切片越界），不是"偶发脏像素"。
//
// 所以缓存按 key 存一个 sync.Pool：一次渲染独占一份（faceScope），结束时
// 归还；顺序调用之间复用同一批 face，并发调用各拿各的。
// face 在进程生命周期内不 Close——它只引用启动期解析好的 *opentype.Font。
type faceCache struct {
	set *FontSet
	mu  sync.Mutex
	m   map[sizedFaceKey]*sync.Pool
}

func newFaceCache(set *FontSet) *faceCache {
	return &faceCache{set: set, m: make(map[sizedFaceKey]*sync.Pool, 8)}
}

func (c *faceCache) pool(k sizedFaceKey) *sync.Pool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[k]
	if !ok {
		p = new(sync.Pool)
		c.m[k] = p
	}
	return p
}

// faceScope 是一次渲染独占的 face 集合。零值不可用，由 faceCache.scope 构造。
type faceScope struct {
	cache *faceCache
	faces map[sizedFaceKey]font.Face
}

func (c *faceCache) scope() *faceScope {
	return &faceScope{cache: c, faces: make(map[sizedFaceKey]font.Face, 8)}
}

func (s *faceScope) face(fam Family, w Weight, size float64) (font.Face, error) {
	k := sizedFaceKey{family: fam, weight: w, size: int(math.Round(size))}
	if f, ok := s.faces[k]; ok {
		return f, nil
	}

	p := s.cache.pool(k)
	if v := p.Get(); v != nil {
		f := v.(font.Face)
		s.faces[k] = f
		return f, nil
	}

	src, err := s.cache.set.Font(k.family, k.weight)
	if err != nil {
		return nil, err
	}
	f, err := opentype.NewFace(src, &opentype.FaceOptions{
		Size:    float64(k.size),
		DPI:     72,
		Hinting: font.HintingNone,
	})
	if err != nil {
		return nil, fmt.Errorf("render: 构造 %s/%s/%dpx face: %w", fam, w, k.size, err)
	}
	s.faces[k] = f
	return f, nil
}

func (s *faceScope) release() {
	for k, f := range s.faces {
		s.cache.pool(k).Put(f)
	}
}

// ---- 单遍布局器 ----

// layout 持有一次的纵向游标与横向区间。img 为 nil 时是测量遍——所有绘制
// 调用都会退化成空操作，但 y 的推进与绘制遍完全相同。
type layout struct {
	faces *faceScope
	src   []byte
	img   *image.RGBA

	x, right   float64
	y          float64
	prevBottom float64
	started    bool
}

func newLayout(faces *faceScope, src string) *layout {
	return &layout{
		faces: faces,
		src:   []byte(src),
		x:     padX,
		right: imgWidth - padX,
		y:     padY,
	}
}

func (l *layout) blocks(ctx context.Context, parent ast.Node) error {
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		// 每处理一个块检查一次：荒谬长的输入要在撞上高度上限之前就能被打断。
		if err := ctx.Err(); err != nil {
			return renderTimeout(err)
		}
		if err := l.block(n); err != nil {
			return err
		}
		// 安全阀：NeedHeight 要的是"至少多高"，已经测到的下界就够插件决定拆几条。
		if bottom := l.y + padY; bottom > hardHeightCap {
			return &host.RenderTooLargeError{NeedHeight: int(math.Ceil(bottom))}
		}
	}
	return nil
}

func (l *layout) block(n ast.Node) error {
	switch v := n.(type) {
	case *ast.Heading:
		return l.heading(v)
	case *ast.Paragraph:
		return l.paragraph(v)
	case *ast.TextBlock:
		return l.paragraph(v)
	case *ast.HTMLBlock:
		// 未识别的块级语法按纯文本原样输出，而不是静默丢掉读者要看的内容。
		return l.paragraphLines(l.literalLines(v.Lines()))
	case *ast.ThematicBreak:
		l.hr()
		return nil
	default:
		// 其余块（列表、引用、代码块、表格……）留在后续 ticket；
		// 递归子块至少不会把整篇文档吞掉。
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if err := l.block(c); err != nil {
				return err
			}
		}
		return nil
	}
}

func (l *layout) heading(n *ast.Heading) error {
	size, lineH := fsH3, lhH3
	switch n.Level {
	case 1:
		size, lineH = fsH1, lhH1
	case 2:
		size, lineH = fsH2, lhH2
	}

	l.gap(mtHeading)
	if err := l.textBlock(collectSpans(n, l.src), WeightBold, size, lineH); err != nil {
		return err
	}
	if n.Level <= 2 {
		// GitHub 的 h1/h2 有一条 border-bottom，位置在行盒下方 0.3em。
		l.y += size * 0.3
		l.fillRect(l.x, l.y, l.right-l.x, borderW, colBorderM)
		l.y += borderW
	}
	l.bottom(mbHeading)
	return nil
}

func (l *layout) paragraph(n ast.Node) error {
	return l.paragraphLines(collectSpans(n, l.src))
}

func (l *layout) paragraphLines(sp []span) error {
	l.gap(0)
	if err := l.textBlock(sp, WeightRegular, fsBody, lhBody); err != nil {
		return err
	}
	l.bottom(mbPara)
	return nil
}

func (l *layout) hr() {
	l.gap(mtHr)
	l.fillRect(l.x, l.y, l.right-l.x, hrH, colBorder)
	l.y += hrH
	l.bottom(mbHr)
}

// textBlock 把一组 span 折行、逐行推进并绘制。行高与字号由调用方给，行内
// 字族仍按字符逐字选择。
func (l *layout) textBlock(sp []span, w Weight, size, lineH float64) error {
	gs, err := l.glyphs(sp, w, size)
	if err != nil {
		return err
	}
	fallback, err := l.faces.face(FontSans, w, size)
	if err != nil {
		return err
	}
	for _, line := range l.wrap(gs, l.right-l.x) {
		l.drawLine(line, l.x, l.y, lineH, fallback)
		l.y += lineH
	}
	return nil
}

// literalLines 把块级未识别语法（HTML）的源码行原样取出。
func (l *layout) literalLines(segs *text.Segments) []span {
	if segs == nil {
		return nil
	}
	var out []span
	for i := 0; i < segs.Len(); i++ {
		if i > 0 {
			out = append(out, span{br: true})
		}
		seg := segs.At(i)
		line := strings.TrimRight(string(seg.Value(l.src)), "\r\n")
		out = append(out, span{text: line})
	}
	return out
}

// gap 实现 margin collapsing：相邻块的间距取两者 margin 的**较大值**。
// 相加会让段间距翻倍，是"看起来不像 GitHub"最常见的成因。
func (l *layout) gap(top float64) {
	if !l.started {
		l.started = true
		return
	}
	l.y += math.Max(l.prevBottom, top)
	l.prevBottom = 0
}

func (l *layout) bottom(m float64) { l.prevBottom = m }

// ---- 行内 span 与字形 ----

// span 是一段同源的纯文本。本 ticket 行内一律按纯文本处理，样式留给后续
// ticket，所以这里只有文本与硬换行两种形态。
type span struct {
	text string
	br   bool
}

func collectSpans(n ast.Node, src []byte) []span {
	var out []span
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Text:
			out = append(out, span{text: string(v.Value(src))})
			if v.HardLineBreak() {
				out = append(out, span{br: true})
			} else if v.SoftLineBreak() {
				// 软换行在 HTML 里就是一个空格：折行器需要它作为断点。
				out = append(out, span{text: " "})
			}
		case *ast.String:
			out = append(out, span{text: string(v.Value)})
		case *ast.AutoLink:
			// 自动链接没有子节点，写回它的标签文本，否则整段链接凭空消失。
			out = append(out, span{text: string(v.Label(src))})
		case *ast.RawHTML:
			// 内嵌 HTML 没有子节点，取原文，避免"内容被吃掉"。
			if v.Segments != nil {
				for i := 0; i < v.Segments.Len(); i++ {
					seg := v.Segments.At(i)
					out = append(out, span{text: string(seg.Value(src))})
				}
			}
		default:
			out = append(out, collectSpans(c, src)...)
		}
	}
	return out
}

type glyph struct {
	r   rune
	f   font.Face
	col color.Color
	br  bool
	adv float64
}

// glyphs 把 span 拍平成逐字符的字形。字族按字符选：CJK 走 FontSC，
// 其余走 FontSans——不是"整段用一个字体再回退"。
func (l *layout) glyphs(sp []span, w Weight, size float64) ([]glyph, error) {
	var out []glyph
	for _, s := range sp {
		if s.br {
			out = append(out, glyph{br: true})
			continue
		}
		for _, r := range s.text {
			f, err := l.faces.face(familyOf(r), w, size)
			if err != nil {
				return nil, err
			}
			adv, ok := f.GlyphAdvance(r)
			width := float64(adv) / 64
			if !ok {
				// 字体里没有这个字形：给个半角宽度，别让整行挤成一团。
				width = size * 0.5
			}
			out = append(out, glyph{r: r, f: f, col: colFg, adv: width})
		}
	}
	return out, nil
}

// familyOf 是"逐字符选字族"的唯一决策点。
func familyOf(r rune) Family {
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

// ---- 折行 ----

// CJK 禁则：这些字符不能出现在行首 / 行尾。直引号与弯引号都收，
// 因为两种写法在真实文档里都出现。
var (
	noBreakBefore = "、。，．：；！？）〕】》」』”’\"'%·…"
	noBreakAfter  = "（〔【《「『“‘\"'"
)

func canBreak(a, b rune) bool {
	if strings.ContainsRune(noBreakBefore, b) || strings.ContainsRune(noBreakAfter, a) {
		return false
	}
	if a == ' ' {
		return true
	}
	if isCJK(a) || isCJK(b) {
		return true
	}
	// 西文只在连字符后可断（after-hyphen 是合法的断点）。
	return a == '-'
}

func (l *layout) wrap(gs []glyph, maxW float64) [][]glyph {
	var lines [][]glyph
	var cur []glyph
	width := 0.0
	lastBreak := -1

	// 段首的空格（含 markdown 里为对齐写的空格）不进图。
	for len(gs) > 0 && !gs[0].br && gs[0].r == ' ' {
		gs = gs[1:]
	}

	flush := func() {
		if len(cur) > 0 {
			lines = append(lines, cur)
		}
		cur, width, lastBreak = nil, 0, -1
	}

	for _, g := range gs {
		if g.br {
			flush()
			continue
		}
		if width+g.adv > maxW && len(cur) > 0 {
			cut := lastBreak
			if cut <= 0 {
				// 整行没有任何合法断点：只能在字符间断开，否则会溢出画布。
				cut = len(cur)
			}
			lines = append(lines, cur[:cut])
			rest := append([]glyph{}, cur[cut:]...)
			// 断点落在空格上时，行首的空格丢弃。
			for len(rest) > 0 && rest[0].r == ' ' {
				rest = rest[1:]
			}
			cur = rest
			width, lastBreak = 0, -1
			for i, x := range cur {
				width += x.adv
				if i >= 1 && canBreak(cur[i-1].r, cur[i].r) {
					lastBreak = i
				}
			}
		}
		cur = append(cur, g)
		width += g.adv
		if len(cur) >= 2 && canBreak(cur[len(cur)-2].r, cur[len(cur)-1].r) {
			lastBreak = len(cur) - 1
		}
	}
	flush()
	if len(lines) == 0 {
		// 空段落也是块：占一行高，否则 height 会与绘制遍错位。
		lines = [][]glyph{{}}
	}
	return lines
}

// ---- 绘制基础件 ----

func (l *layout) fillRect(x, y, w, h float64, c color.Color) {
	if l.img == nil || w <= 0 || h <= 0 {
		return
	}
	dr := image.Rect(int(math.Round(x)), int(math.Round(y)),
		int(math.Round(x+w)), int(math.Round(y+h)))
	draw.Draw(l.img, dr, image.NewUniform(c), image.Point{}, draw.Over)
}

func (l *layout) text(s string, f font.Face, c color.Color, x, baseline float64) float64 {
	if s == "" {
		return x
	}
	w := measureString(f, s)
	if l.img != nil {
		d := font.Drawer{
			Dst:  l.img,
			Src:  image.NewUniform(c),
			Face: f,
			Dot: fixed.Point26_6{
				X: fixed.Int26_6(math.Round(x * 64)),
				Y: fixed.Int26_6(math.Round(baseline * 64)),
			},
		}
		d.DrawString(s)
	}
	return x + w
}

func measureString(f font.Face, s string) float64 {
	d := font.Drawer{Face: f}
	return float64(d.MeasureString(s)) / 64
}

// drawLine 按**这一行实际使用的 face** 的 Metrics 定位基线，而不是把文字
// 垂直居中：CSS 的行盒高度不等分，ascent/descent 的比例由字体决定。
// 用正文字体给标题定基线会让标题整体偏移。
func (l *layout) drawLine(gs []glyph, x, y, lineH float64, fallback font.Face) {
	baseline := baselineOf(lineFace(gs, fallback), y, lineH)

	cx := x
	for i := 0; i < len(gs); {
		j := i
		var sb strings.Builder
		for j < len(gs) && gs[j].f == gs[i].f && gs[j].col == gs[i].col {
			sb.WriteRune(gs[j].r)
			j++
		}
		cx = l.text(sb.String(), gs[i].f, gs[i].col, cx, baseline)
		i = j
	}
}

// lineFace 取行内第一个非空格字形所属的 face。混排行（中英混排）里各字族
// 的 ascent/descent 只差零点几个像素，取首个即为该行的"主字体"。
func lineFace(gs []glyph, fallback font.Face) font.Face {
	for _, g := range gs {
		if !g.br && g.r != ' ' {
			return g.f
		}
	}
	return fallback
}

func baselineOf(f font.Face, y, lineH float64) float64 {
	m := f.Metrics()
	asc := float64(m.Ascent) / 64
	desc := float64(m.Descent) / 64
	return y + (lineH-(asc+desc))/2 + asc
}

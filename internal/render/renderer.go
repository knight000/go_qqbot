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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
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
	canvas.log = r.log
	if err := canvas.blocks(ctx, doc); err != nil {
		return host.Image{}, err
	}
	canvas.warnMissing()

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
	log   *slog.Logger

	x, right   float64
	y          float64
	prevBottom float64
	started    bool

	// muted 表示当前处于块引用内：文字整体降一档亮度（GitHub 的
	// blockquote 用 --fgColor-muted）。
	muted bool
	// nested 是列表嵌套深度：嵌套列表的 margin-top 为 0。
	nested int
	// items 非 nil 时逐项记录列表项的块级几何。缩进漂移是几何量：记下
	// 每项的起始 x 就能断言兄弟项没有互相影响，不必比对像素。只有测试会
	// 设置它，正常渲染路径为 nil。
	items *[]listItemGeom
	// missing 记录本遍布局遇到的缺字。只在绘制遍上报一次 warn。
	missing map[rune]struct{}
}

func newLayout(faces *faceScope, src string) *layout {
	return &layout{
		faces:   faces,
		src:     []byte(src),
		x:       padX,
		right:   imgWidth - padX,
		y:       padY,
		missing: make(map[rune]struct{}),
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
	case *ast.List:
		return l.list(v)
	case *ast.Blockquote:
		return l.blockquote(v)
	case *ast.FencedCodeBlock:
		return l.codeBlock(v)
	case *ast.CodeBlock:
		return l.codeBlock(v)
	case *east.Table:
		return l.table(v)
	default:
		// 其余块（图片块、脚注等 v1 不支持的语法）递归子块，
		// 至少不会把整篇文档吞掉。
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
//
// 左边界在进入函数时就固定成本地变量 x：药丸 padding、折行、续行都只作用于
// 行内游标，块级 l.x 在整个段落里不变。这样"一个段落画了几行"就不可能改变
// 后面兄弟块的横向位置——列表标记列依赖的正是块级 l.x。
func (l *layout) textBlock(sp []span, w Weight, size, lineH float64) error {
	gs, err := l.glyphs(sp, w, size)
	if err != nil {
		return err
	}
	fallback, err := l.faces.face(FontSans, w, size)
	if err != nil {
		return err
	}
	x := l.x
	for _, line := range l.wrap(gs, l.right-x) {
		l.drawLine(line, x, l.y, lineH, fallback)
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

// noteMissing 记录一个子集外的字符。整篇画完才统一打一条 warn：一行生僻字
// 不该刷出几十条日志，也绝不能变成 error——出一张带方框的图比什么都看不到
// 更接近读者的意图。
func (l *layout) noteMissing(r rune) {
	if l.missing == nil {
		return
	}
	l.missing[r] = struct{}{}
}

func (l *layout) warnMissing() {
	if l.log == nil || len(l.missing) == 0 {
		return
	}
	runes := make([]rune, 0, len(l.missing))
	for r := range l.missing {
		runes = append(runes, r)
	}
	sort.Slice(runes, func(i, j int) bool { return runes[i] < runes[j] })

	const maxList = 8
	var sb strings.Builder
	for i, r := range runes {
		if i == maxList {
			fmt.Fprintf(&sb, " 等 %d 个", len(runes))
			break
		}
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%q(U+%04X)", r, r)
	}
	l.log.Warn("渲染遇到字体子集外的字符，已画成方框",
		"count", len(runes), "runes", sb.String())
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
	return l.wrapMode(gs, maxW, false)
}

// wrapMode 是唯一的折行实现。keepIndent 为真时保留行首空格——代码块的
// 缩进是内容的一部分，按正文那样吃掉会让代码失去层级。
func (l *layout) wrapMode(gs []glyph, maxW float64, keepIndent bool) [][]glyph {
	var lines [][]glyph
	var cur []glyph
	width := 0.0
	lastBreak := -1

	// 段首的空格（含 markdown 里为对齐写的空格）不进图；代码块例外。
	if !keepIndent {
		for len(gs) > 0 && !gs[0].br && gs[0].r == ' ' {
			gs = gs[1:]
		}
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
			// 断点落在空格上时，行首的空格丢弃（代码块保留）。
			if !keepIndent {
				for len(rest) > 0 && rest[0].r == ' ' {
					rest = rest[1:]
				}
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

// roundRect 画圆角矩形。逐行扫描而不是逐像素：边角只有十几行，代价可以
// 忽略，换来的是不引任何绘图库。
func (l *layout) roundRect(x, y, w, h, rad float64, c color.Color) {
	if l.img == nil || w <= 0 || h <= 0 {
		return
	}
	if rad <= 0 {
		l.fillRect(x, y, w, h, c)
		return
	}
	for row := 0; row < int(h); row++ {
		fy := float64(row) + 0.5
		dy := 0.0
		if fy < rad {
			dy = rad - fy
		} else if fy > h-rad {
			dy = fy - (h - rad)
		}
		inset := 0.0
		if dy > 0 {
			inset = rad - math.Sqrt(math.Max(0, rad*rad-dy*dy))
		}
		l.fillRect(x+inset, y+float64(row), w-2*inset, 1, c)
	}
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

package render

import "image/color"

// 视觉基线：GitHub markdown 的 **2× 换算**（浏览器 540 CSS px 宽 → 1080 设备像素）。
//
// 所有数值只在这里出现一次：布局器、测试与将来的文档都读这里，避免"文档里
// 抄了一份、代码改了文档没改"的漂移。颜色与字号来自 Primer light token，
// 见 docs/adr/0002-render-pure-go-raster.md。
const (
	// imgWidth 是固定画布宽度。宽度不随内容变化——"不同插件出的图一样宽"
	// 是这套渲染器看起来统一的前提。
	imgWidth = 1080.0
	// maxHeight 是允许出图的最大高度。1080×4000 的 RGBA 约 17.3 MB，
	// 与并发上限 2 一起把峰值内存压在 MemoryMax=256M 之内。
	maxHeight = 4000
	// hardHeightCap 是测量阶段的安全阀：总高要准确才能报出 NeedHeight，
	// 所以第一遍通常跑完；但荒谬长的输入不必真的走到尽头，超过这个值就
	// 用已测到的下界提前中止。
	hardHeightCap = 100000.0

	// maxConcurrentRenders 是进程级的渲染并发上限。
	//
	// 不设限时 16 个 worker 峰值 277 MB，会被内核直接杀掉——这是整个特性里
	// 唯一能造成生产事故的数字。1080×4000 的 RGBA = 17.3 MB，两个并发约
	// 35 MB，含 PNG 编码缓冲峰值约 50 MB。
	maxConcurrentRenders = 2

	padX = 32.0
	padY = 32.0

	fsBody  = 32.0
	lhBody  = 48.0
	fsH1    = 64.0
	lhH1    = 80.0
	fsH2    = 48.0
	lhH2    = 62.0
	fsH3    = 40.0
	lhH3    = 52.0
	fsCode  = 27.0
	lhCode  = 39.0
	fsSmall = 24.0

	mtHeading = 48.0
	mbHeading = 32.0
	mbPara    = 32.0
	mbCode    = 32.0
	mtHr      = 48.0
	mbHr      = 48.0
	hrH       = 8.0

	codePad    = 32.0
	codeRadius = 12.0
	listIndent = 64.0
	liGap      = 8.0
	quoteBar   = 8.0
	quotePad   = 32.0
	tablePadV  = 12.0
	tablePadH  = 26.0
	borderW    = 2.0
)

// 颜色。全部是不透明的 Primer token；半透明色必须用 color.NRGBA，
// 见 colNeutBg 的注释。
var (
	colFg      = color.RGBA{0x1f, 0x23, 0x28, 0xff}
	colMuted   = color.RGBA{0x59, 0x63, 0x6e, 0xff}
	colBorder  = color.RGBA{0xd0, 0xd7, 0xde, 0xff}
	colBorderM = color.RGBA{0xd8, 0xde, 0xe4, 0xff}
	colMutedBg = color.RGBA{0xf6, 0xf8, 0xfa, 0xff}
	colAccent  = color.RGBA{0x09, 0x69, 0xda, 0xff}

	// 必须是 NRGBA：color.RGBA 是 alpha 预乘语义，写 {175,184,193,0x33} 会被
	// 当成预乘值再叠一次，合成结果直接溢出成 #7c858e（实测），而不是 #f0f1f2。
	colNeutBg = color.NRGBA{0x81, 0x8b, 0x98, 0x1f} // --bgColor-neutral-muted: #818b981f
)

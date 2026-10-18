package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/yuin/goldmark/text"
)

// sampleDirEnv 是样图开关：设置后测试把出图写到该目录，便于人工看观感。
// 不设置时测试不产生任何文件。
const sampleDirEnv = "RENDER_SAMPLE_DIR"

// 渲染器在多个用例间共享：字体解析约 300 ms，每个用例重来一遍会让
// `go test ./internal/render` 变成分钟级。face 缓存本身是并发安全的，
// 共享实例同时也是这条性质的间接验证。
var (
	sharedOnce sync.Once
	sharedR    *Renderer
	sharedErr  error
)

func testRenderer(t *testing.T) *Renderer {
	t.Helper()
	sharedOnce.Do(func() {
		set, err := LoadFonts()
		if err != nil {
			sharedErr = err
			return
		}
		sharedR = New(set, nil)
	})
	if sharedErr != nil {
		t.Fatalf("LoadFonts: %v", sharedErr)
	}
	return sharedR
}

func renderPNG(t *testing.T, src string) []byte {
	t.Helper()
	img, err := testRenderer(t).Markdown(context.Background(), src)
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	if !img.Valid() {
		t.Fatal("Image.Valid() = false，期望 true")
	}
	return img.Data()
}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	m, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png.Decode: %v", err)
	}
	return m
}

const sampleDoc = `# 构建汇总 · 2026-10-18

本次构建覆盖三个平台，其中 linux/amd64 与 windows/amd64 通过，linux/arm64 因交叉编译链
缺失而跳过，详细产物见发布页。

## 服务状态

### 备注

段落与段落之间的间距只取较大值，因为两个 margin 会折叠成一个，而不是相加。

---

审核人：knight　｜　签发：自动流水线
`

// 只有段落、H1–H3 与分隔线的文档必须能画出一张合法 PNG。
func TestMarkdownRendersSupportedBlocks(t *testing.T) {
	data := renderPNG(t, sampleDoc)

	m := decodePNG(t, data)
	if got := m.Bounds().Dx(); got != int(imgWidth) {
		t.Errorf("宽 = %d，期望 %v（宽度不随内容变化）", got, imgWidth)
	}
	if got := m.Bounds().Dy(); got <= 0 {
		t.Fatalf("高 = %d，期望 > 0", got)
	}

	writeSample(t, "supported-blocks.png", data)
}

// 空文档不是错误：出一张只含上下留白的最小图。
func TestMarkdownEmptyDocument(t *testing.T) {
	for _, src := range []string{"", "  \n\n\t\n"} {
		data := renderPNG(t, src)
		m := decodePNG(t, data)
		if h := m.Bounds().Dy(); h <= 0 {
			t.Errorf("空文档高 = %d，期望 > 0", h)
		}
		if w := m.Bounds().Dx(); w != int(imgWidth) {
			t.Errorf("空文档宽 = %d，期望 %v", w, imgWidth)
		}
	}
}

// writeSample 只在 RENDER_SAMPLE_DIR 被设置时落盘——样图是人工自查手段，
// 默认跑测试不得产生任何文件。
func writeSample(t *testing.T, name string, data []byte) {
	t.Helper()
	dir := os.Getenv(sampleDirEnv)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建样图目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("写样图: %v", err)
	}
	t.Logf("样图已写入 %s", filepath.Join(dir, name))
}

// 超长内容不是"画一张 8000px 的图"，而是带实测高度拒绝。
func TestMarkdownTooLarge(t *testing.T) {
	src := strings.Repeat("这是一段用于把画布拉长的中文内容，它最终必须超过四千像素的高度。\n\n", 100)

	_, err := testRenderer(t).Markdown(context.Background(), src)
	if !errors.Is(err, host.ErrRenderTooLarge) {
		t.Fatalf("err = %v，期望 errors.Is(err, host.ErrRenderTooLarge)", err)
	}
	var tooLarge *host.RenderTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("errors.As 取不到 *host.RenderTooLargeError：%v", err)
	}
	if tooLarge.NeedHeight <= maxHeight {
		t.Errorf("NeedHeight = %d，期望 > %d（插件据此决定拆几条）", tooLarge.NeedHeight, maxHeight)
	}
}

// 已取消的 ctx 不该出图，也要能被 errors.Is 命中哨兵。
func TestMarkdownCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := testRenderer(t).Markdown(ctx, "正文")
	if !errors.Is(err, host.ErrRenderTimeout) {
		t.Fatalf("err = %v，期望 errors.Is(err, host.ErrRenderTimeout)", err)
	}
}

// 拿不到并发槽时也必须能被取消：插件 handler 只有 3 秒预算，排队算在里面。
func TestMarkdownCanceledWhileWaitingForSlot(t *testing.T) {
	// 直接占满包级信号量——这是"确定拿不到槽"的唯一可靠构造方式。
	for i := 0; i < maxConcurrentRenders; i++ {
		renderSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < maxConcurrentRenders; i++ {
			<-renderSlots
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := testRenderer(t).Markdown(ctx, "正文")
	if !errors.Is(err, host.ErrRenderTimeout) {
		t.Fatalf("err = %v，期望排队期间被取消时返回 errors.Is(err, host.ErrRenderTimeout)", err)
	}
}

// 16 个并发渲染全部成功：这是 face 缓存与包级信号量的并发正确性锁。
func TestMarkdownConcurrentCallsAllSucceed(t *testing.T) {
	r := testRenderer(t)

	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					errs[i] = fmt.Errorf("panic: %v", p)
				}
			}()
			img, err := r.Markdown(context.Background(), sampleDoc)
			if err != nil {
				errs[i] = err
				return
			}
			if !img.Valid() {
				errs[i] = errors.New("Image.Valid() = false")
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("第 %d 个并发调用失败：%v", i, err)
		}
	}
}

// 白盒检查折行结果：行首不出现禁则字符，行尾不出现禁则字符。
func TestChineseWrapRespectsForbiddenBreaks(t *testing.T) {
	src := strings.Repeat("中文标点必须遵循禁则，例如逗号、句号。括号（这里）也一样！还有「引号」。", 12)

	l := newLayout(testRenderer(t).faces.scope(), src)
	doc := markdown.Parser().Parse(text.NewReader([]byte(src)))
	gs, err := l.glyphs(collectSpans(doc.FirstChild(), l.src), WeightRegular, fsBody)
	if err != nil {
		t.Fatalf("glyphs: %v", err)
	}
	lines := l.wrap(gs, l.right-l.x)
	if len(lines) < 3 {
		t.Fatalf("折行只得到 %d 行，样例不够长，测不出禁则", len(lines))
	}

	for i, line := range lines {
		first, ok := firstRune(line)
		if !ok {
			continue
		}
		if strings.ContainsRune(noBreakBefore, first) {
			t.Errorf("第 %d 行以禁则字符 %q 开头", i, first)
		}
		last, _ := lastRune(line)
		if strings.ContainsRune(noBreakAfter, last) {
			t.Errorf("第 %d 行以禁则字符 %q 结尾", i, last)
		}
	}
}

func firstRune(gs []glyph) (rune, bool) {
	for _, g := range gs {
		if !g.br && g.r != ' ' {
			return g.r, true
		}
	}
	return 0, false
}

func lastRune(gs []glyph) (rune, bool) {
	for i := len(gs) - 1; i >= 0; i-- {
		if !gs[i].br && gs[i].r != ' ' {
			return gs[i].r, true
		}
	}
	return 0, false
}

// 相邻块的间距取两个 margin 的**较大值**。用高度差断言：段落接分隔线时
// 间距应当是 mtHr，而不是 mbPara + mtHr。
func TestAdjacentMarginsCollapseToLarger(t *testing.T) {
	if mtHr <= mbPara {
		t.Fatalf("用例前提不成立：需要 mtHr(%v) > mbPara(%v)", mtHr, mbPara)
	}
	r := testRenderer(t)
	height := func(src string) int {
		img, err := r.Markdown(context.Background(), src)
		if err != nil {
			t.Fatalf("Markdown(%q): %v", src, err)
		}
		return decodePNG(t, img.Data()).Bounds().Dy()
	}

	base := height("正文一段")
	withRule := height("正文一段\n\n---\n")

	want := int(mtHr + hrH)
	if got := withRule - base; got != want {
		t.Errorf("段落与分隔线的间距+线高 = %d，期望 %d（取较大值 %v）", got, want, mtHr)
	}
	if summed := int(mbPara + mtHr + hrH); summed == want {
		t.Errorf("用例前提不成立：相加与取最大值得到同一个数 %d", summed)
	}
}

// 未识别语法按纯文本原样输出：不报错，也不能把内容静默吃掉。
func TestUnrecognizedSyntaxRendersAsPlainText(t *testing.T) {
	broken := "残缺的 **加粗 标记，后面没有闭合。"
	data := renderPNG(t, broken+"\n\n行内 <span>标签</span> 也在，应当照原样出现。")

	m := decodePNG(t, data)
	if w := m.Bounds().Dx(); w != int(imgWidth) {
		t.Errorf("宽 = %d，期望 %v", w, imgWidth)
	}

	// 块级 HTML 有内容可画：高度必须大于"什么都没有"。
	html := decodePNG(t, renderPNG(t, "<div class=\"note\">内嵌 HTML 段落</div>\n"))
	empty := decodePNG(t, renderPNG(t, ""))
	if html.Bounds().Dy() <= empty.Bounds().Dy() {
		t.Errorf("HTML 块高度 %d 不大于空文档 %d，内容很可能被丢掉了",
			html.Bounds().Dy(), empty.Bounds().Dy())
	}
}

// 不设 RENDER_SAMPLE_DIR 时渲染不得产生任何文件。
func TestRenderWritesNoFilesByDefault(t *testing.T) {
	if os.Getenv(sampleDirEnv) != "" {
		t.Skipf("设置了 %s，本用例只在默认配置下有意义", sampleDirEnv)
	}

	dir := t.TempDir()
	t.Chdir(dir)

	renderPNG(t, sampleDoc)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读工作目录: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("渲染产生了文件 %v，期望不落盘", names)
	}
}

// 平台下发的是单独 CR（2026-10-19 raw 轨迹取证）。CR 与 CRLF 必须和 LF 出图
// 逐字节一致——归一化前整段被当成一个 ATX 标题画成一行（166 px，多行是 466 px）。
func TestMarkdownNormalizesLineEndings(t *testing.T) {
	const lf = "# 标题\n## 二级标题\n\n```\nimport os\nreturn\n```"
	want := renderPNG(t, lf)
	for _, c := range []struct{ name, src string }{
		{"CR", strings.ReplaceAll(lf, "\n", "\r")},
		{"CRLF", strings.ReplaceAll(lf, "\n", "\r\n")},
	} {
		got := renderPNG(t, c.src)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s 换行与 LF 出图不一致（%d vs %d 字节）", c.name, len(got), len(want))
		}
	}
}

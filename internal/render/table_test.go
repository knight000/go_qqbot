package render

import (
	"strconv"
	"strings"
	"testing"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

const tableSample = `| 服务 | 版本 | 状态 | 最近检查 | 备注 |
| --- | --- | --- | --- | --- |
| qqbot | 0.9.4 | 运行中 | 20:41 | 主进程，重启后首次 ` + "`/ping`" + ` 会包含启动耗时 |
| frps | 0.71.0 | 运行中 | 20:40 | 隧道入口 |
| aria2 | 1.37.0 | 已停止 | 19:12 | 手动停用，等待带宽窗口 |
| webdav | — | 降级 | 18:55 | 证书还剩 3 天 |

| 指标 | 1月 | 2月 | 3月 | 4月 | 5月 | 6月 |
| --- | --- | --- | --- | --- | --- | --- |
| 消息量 | 12k | 13k | 15k | 14k | 18k | 21k |
| 失败率 | 0.4% | 0.3% | 0.2% | 0.5% | 0.2% | 0.1% |
`

// 表格要按网格排版，而不是把每个单元格当独立段落竖着摞起来。
// 32 个单元格竖排至少有 32×(48+32) ≈ 2560px 高；网格排版只有几行。
func TestTableLaysOutAsGrid(t *testing.T) {
	src := "| 一 | 二 | 三 | 四 | 五 | 六 | 七 | 八 |\n" +
		"| --- | --- | --- | --- | --- | --- | --- | --- |\n" +
		"| 甲 | 乙 | 丙 | 丁 | 戊 | 己 | 庚 | 辛 |\n" +
		"| 子 | 丑 | 寅 | 卯 | 辰 | 巳 | 午 | 未 |\n"

	h := imgHeight(t, src)
	if h >= 1000 {
		t.Errorf("八列表格高 %d，接近逐单元格竖排的高度，表格很可能没有按列宽排", h)
	}
}

// wideTableDoc 构造 ncol 列、每格都是 cell 的表格。
//
// 列多到一定数目后，内边距的下限（4px）与列宽的下限（1px）相加本身就超过
// 可用宽度：ncol×9 > avail 时表格必然溢出，右边框被画布裁掉。
func wideTableDoc(ncol int, cell string) string {
	var sb strings.Builder
	sb.WriteString("|")
	for i := 0; i < ncol; i++ {
		sb.WriteString(" " + cell + " |")
	}
	sb.WriteString("\n|")
	for i := 0; i < ncol; i++ {
		sb.WriteString(" --- |")
	}
	for row := 0; row < 2; row++ {
		sb.WriteString("\n|")
		for i := 0; i < ncol; i++ {
			sb.WriteString(" " + cell + " |")
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// 无论多少列，表格总宽都不得超过可用宽度：溢出的部分会被画布裁掉，而读者
// 从图上看不出后面还有列。列宽被压到亚像素也比"内容静默消失"强。
func TestWideTableNeverExceedsAvailableWidth(t *testing.T) {
	for _, ncol := range []int{8, 16, 40, 113, 130, 300} {
		src := wideTableDoc(ncol, "值")

		l := newLayout(testRenderer(t).faces.scope(), src)
		head, body, n := tableCells(parseTable(t, src), l.src)
		if n != ncol {
			t.Fatalf("%d 列表格解析出 %d 列", ncol, n)
		}
		tl, err := l.layoutTable(head, body, n)
		if err != nil {
			t.Fatalf("%d 列 layoutTable: %v", ncol, err)
		}

		avail := l.right - l.x
		if tl.total > avail+0.5 {
			t.Errorf("%d 列表格总宽 %.1f 超过可用宽度 %.1f，右边框会被裁掉", ncol, tl.total, avail)
		}
		for i, w := range tl.widths {
			if w <= 0 {
				t.Errorf("%d 列表格第 %d 列宽 %.3f，不允许退化成零或负宽", ncol, i, w)
			}
		}
	}
}

// 单元格内容超宽时在单元格内折行，而不是把表格撑出画布。
func TestTableWrapsWideCells(t *testing.T) {
	short := "| 服务 | 状态 |\n| --- | --- |\n| qqbot | 运行中 |\n"
	wide := "| 服务 | 状态 |\n| --- | --- |\n| qqbot | " + strings.Repeat("很长的状态说明", 24) + " |\n"

	if imgHeight(t, wide) <= imgHeight(t, short) {
		t.Errorf("超宽单元格没有折行：宽表与窄表高度相同")
	}
}

// 列数多到内边距本身就超过画布宽度时也不能崩：收窄内边距、继续折行，
// 保证表格仍在 1080 之内。
func TestVeryWideTableStillRenders(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("| 列")
	}
	sb.WriteString(" |\n|")
	for i := 0; i < 20; i++ {
		sb.WriteString(" --- |")
	}
	sb.WriteString("\n|")
	for i := 0; i < 20; i++ {
		sb.WriteString(" 数值 |")
	}
	sb.WriteString("\n")

	h := imgHeight(t, sb.String())
	if h <= 0 || h > maxHeight {
		t.Errorf("二十列表格高 %d，期望 (0, %d]", h, maxHeight)
	}
}

// 多列表格、单元格内行内代码、斑马纹区域都能正常出图。
func TestTableRendersWithBordersAndStripes(t *testing.T) {
	data := renderPNG(t, tableSample)
	m := decodePNG(t, data)
	if got := m.Bounds().Dx(); got != int(imgWidth) {
		t.Errorf("宽 = %d，期望 %v", got, imgWidth)
	}
	if h := m.Bounds().Dy(); h <= 0 {
		t.Fatalf("高 = %d，期望 > 0", h)
	}
	writeSample(t, "table.png", data)
}

// tableStressDoc 是"一列不可断长串 + 一列长中文说明"的六列表格。
//
// 这是列宽分配的极端情形：不可断串所在列的 min-content 就是它自身的宽度，
// 按 min-content 当下界会让它独占整表；而中文可以逐字断行，说明列的
// min-content 只有一个汉字，于是被压成一字一行。
func tableStressDoc() string {
	// 60 字符、无空格、无连字符：没有任何合法断点，只能硬断。
	token := strings.Repeat("9f86d081", 8)[:60]
	return "| 组件 | 说明 | 版本 | 当前状态 | 负责人 | 备注 |\n" +
		"| --- | --- | --- | --- | --- | --- |\n" +
		"| " + token + " | 这是一段用来测试单元格内折行的中文说明，长度足够把整张表格撑过可用宽度 | 2026.10.18-rc1 | 正在灰度发布中 | 运维值班组 | 无异常，等待下一轮灰度窗口 |\n"
}

// 长 token 不能把别的列挤成竖排：每列都不窄于可读性下限，总宽不超画布，
// 整图高度也必须落在一个"还像表格"的范围里。
func TestTableColumnWidthsKeepColumnsReadable(t *testing.T) {
	src := tableStressDoc()
	l := newLayout(testRenderer(t).faces.scope(), src)
	head, body, ncol := tableCells(parseTable(t, src), l.src)
	if ncol != 6 {
		t.Fatalf("列数 = %d，期望 6", ncol)
	}

	tl, err := l.layoutTable(head, body, ncol)
	if err != nil {
		t.Fatalf("layoutTable: %v", err)
	}

	if avail := l.right - l.x; tl.total > avail+0.5 {
		t.Errorf("表格总宽 %.1f 超过可用宽度 %.1f", tl.total, avail)
	}
	for i, w := range tl.widths {
		if w < tableMinColWidth-0.5 {
			t.Errorf("第 %d 列宽 %.1f 低于可读性下限 %.1f，该列会被压成竖排", i, w, tableMinColWidth)
		}
	}

	// 一字一行的表格高 2337px（实测）；按列宽正常折行时不到它的一半。
	const heightCap = 1200
	if h := imgHeight(t, src); h >= heightCap {
		t.Errorf("图高 %d px ≥ %d，长 token 仍在挤占其它列的宽度", h, heightCap)
	}
	writeSample(t, "table-long-token.png", renderPNG(t, src))
}

// 单元格折行结果不得溢出列宽：长 token 在自己的列里按可用宽度硬断，
// 而不是把整张表撑出画布。
func TestTableLongTokenHardBreaksInsideCell(t *testing.T) {
	src := tableStressDoc()
	l := newLayout(testRenderer(t).faces.scope(), src)
	head, body, ncol := tableCells(parseTable(t, src), l.src)
	tl, err := l.layoutTable(head, body, ncol)
	if err != nil {
		t.Fatalf("layoutTable: %v", err)
	}

	check := func(where string, cells [][]span, bold bool) {
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
				t.Fatalf("%s glyphs: %v", where, err)
			}
			for j, ln := range l.wrap(gs, tl.widths[i]) {
				if gw := glyphsWidth(ln); gw > tl.widths[i]+0.5 {
					t.Errorf("%s第 %d 列第 %d 行宽 %.1f，溢出列宽 %.1f", where, i, j, gw, tl.widths[i])
				}
			}
		}
	}
	check("表头", head, true)
	for r, row := range body {
		check("数据行 "+strconv.Itoa(r), row, false)
	}

	// 长 token 必须被硬断成多行，且每行不止一两个字：前者说明没溢出，
	// 后者说明列宽没有被压到"一字一行"。
	gs, err := l.glyphs(body[0][0], WeightRegular, fsBody)
	if err != nil {
		t.Fatalf("glyphs: %v", err)
	}
	lines := l.wrap(gs, tl.widths[0])
	if len(lines) < 2 {
		t.Errorf("60 字符的不可断 token 只折出 %d 行，列宽被撑到了整表宽度", len(lines))
	}
	for j, ln := range lines {
		if len(ln) < 2 {
			t.Errorf("token 第 %d 行只有 %d 个字符，列宽退化成了单字列", j, len(ln))
		}
	}
}

func parseTable(t *testing.T, src string) *east.Table {
	t.Helper()
	doc := markdown.Parser().Parse(text.NewReader([]byte(src)))
	var found *east.Table
	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if tbl, ok := n.(*east.Table); ok {
			found = tbl
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		t.Fatalf("ast.Walk: %v", err)
	}
	if found == nil {
		t.Fatalf("文档里没有表格：\n%s", src)
	}
	return found
}

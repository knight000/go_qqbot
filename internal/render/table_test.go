package render

import (
	"strings"
	"testing"
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

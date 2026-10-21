package render

import (
	"bytes"
	"flag"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// golden 是入库的基准图，用来锁住三条"只有看图才发现"的视觉结论：
// 松散列表的段间距、行内 tab 不出方框、超宽表格的右边框没有被画布裁掉。
//
// 只裁每条断言关心的那一块，而不是比对整张成品图：整图 diff 会把无关的
// 样式变化也算进来，仓库里也不必存上百 KB 的成品。三条基准合计控制在
// 30 KB 量级。
//
// 光栅全在纯 Go 里（hinting 关闭，字体随包内嵌），同一份 go.sum 下像素是
// 确定的；升级 golang.org/x/image 之后基准可能要重写：
//
//	go test ./internal/render -run Golden -update-golden
const goldenDir = "testdata/golden"

// updateGolden 只在人工复核后使用：它会把当前输出直接写成基准，
// 等于"认可现在看到的样子"。
var updateGolden = flag.Bool("update-golden", false, "重写 testdata/golden 下的基准图")

// goldenCrop 把 m 的 r 区域与基准图逐像素比对。尺寸或像素不同都算失败，
// 并给出首个差异点——够定位，不必把两张图都存下来。
func goldenCrop(t *testing.T, name string, m image.Image, r image.Rectangle) {
	t.Helper()

	r = r.Intersect(m.Bounds())
	if r.Empty() {
		t.Fatalf("裁剪区域 %v 为空（图 %v）", r, m.Bounds())
	}
	crop := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(crop, crop.Bounds(), m, r.Min, draw.Src)

	var buf bytes.Buffer
	if err := png.Encode(&buf, crop); err != nil {
		t.Fatalf("编码裁剪区域: %v", err)
	}

	path := filepath.Join(goldenDir, name)
	if *updateGolden {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("创建基准图目录: %v", err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("写基准图: %v", err)
		}
		t.Logf("已重写基准图 %s（%d 字节）", path, buf.Len())
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读基准图 %s: %v（用 -update-golden 生成）", path, err)
	}
	base := decodePNG(t, want)
	if base.Bounds() != crop.Bounds() {
		t.Fatalf("%s 尺寸 %v，当前 %v（用 -update-golden 复核后重写）",
			name, base.Bounds(), crop.Bounds())
	}

	diff, fx, fy := 0, -1, -1
	baseMin := base.Bounds().Min
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			if crop.RGBAAt(x, y) != color.RGBAModel.Convert(base.At(baseMin.X+x, baseMin.Y+y)) {
				if diff == 0 {
					fx, fy = x, y
				}
				diff++
			}
		}
	}
	if diff > 0 {
		t.Errorf("%s 有 %d/%d 个像素与基准不同，首个差异在 (%d,%d)（用 -update-golden 复核后重写）",
			name, diff, r.Dx()*r.Dy(), fx, fy)
	}
}

// 松散列表两段之间的空隙是 P3-1 的视觉面：两个段落必须能看出是两段，
// 而不是首尾相接（零间距）。
func TestGoldenLooseList(t *testing.T) {
	data := renderPNG(t, "- 第一项\n\n  第二段：松散列表里两段之间必须有段间距。\n\n- 第二项\n")
	m := decodePNG(t, data)
	goldenCrop(t, "loose-list.png", m, image.Rect(0, 0, int(imgWidth), m.Bounds().Dy()))
}

// 行内 tab 是 P3-3 的视觉面：字体里没有 tab 字形，不预处理就是四个缺字
// 方框（还会误报"字体子集外"）。基准里 tab 必须表现为一段空白。
func TestGoldenInlineTab(t *testing.T) {
	m := decodePNG(t, renderPNG(t, "甲\t乙\t丙 与 a\tb\tc\n"))
	goldenCrop(t, "inline-tab.png", m, image.Rect(0, 0, int(imgWidth), m.Bounds().Dy()))
}

// 超宽表格的右边框是 P3-2 的视觉面：130 列时旧的 9px/列下限让总宽 1170px
// 超出 1016px 的可用宽度，右边框与最后几列一起被画布裁掉。
//
// 基准的每格只放一个句点：这条断言看的是"网格的右边框在画布内"，而 130 列
// 汉字在 1080px 宽下每个字都比列宽宽，横竖会糊成一片、看不出边框位置
// （有汉字的场景由 TestWideTableNeverExceedsAvailableWidth 守）。
func TestGoldenWideTableRightEdge(t *testing.T) {
	m := decodePNG(t, renderPNG(t, wideTableDoc(130, ".")))
	goldenCrop(t, "table-wide-right.png", m, image.Rect(900, 0, int(imgWidth), m.Bounds().Dy()))
}

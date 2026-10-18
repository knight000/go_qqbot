package render

import (
	"path/filepath"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

// 测试用的度量尺寸：72 DPI 下 16 px，足够让 hinting 与舍入都发生在真实量级上。
const (
	testSize = 16
	testDPI  = 72
)

// faceOf 用被测字体构造一个 face。face 的构造参数（Size/DPI/Hinting）属于
// 布局器，不属于字体包，所以这里由测试自己给。
func faceOf(t *testing.T, set *FontSet, fam Family, w Weight) font.Face {
	t.Helper()
	src, err := set.Font(fam, w)
	if err != nil {
		t.Fatalf("Font(%s, %d): %v", fam, w, err)
	}
	face, err := opentype.NewFace(src, &opentype.FaceOptions{
		Size:    testSize,
		DPI:     testDPI,
		Hinting: font.HintingNone,
	})
	if err != nil {
		t.Fatalf("NewFace(%s, %d): %v", fam, w, err)
	}
	t.Cleanup(func() { _ = face.Close() })
	return face
}

func TestLoadFontsParsesAllSixFaces(t *testing.T) {
	set, err := LoadFonts()
	if err != nil {
		t.Fatalf("LoadFonts: %v", err)
	}

	for _, fam := range []Family{FontSans, FontSC, FontMono} {
		for _, w := range []Weight{WeightRegular, WeightBold} {
			t.Run(string(fam)+"/"+w.String(), func(t *testing.T) {
				face := faceOf(t, set, fam, w)

				m := face.Metrics()
				if m.Ascent <= 0 {
					t.Errorf("Ascent = %d，期望 > 0", m.Ascent)
				}
				if m.Descent <= 0 {
					t.Errorf("Descent = %d，期望 > 0", m.Descent)
				}
				// 行高为 0 会让布局器算出无限大的行数，是比"字丑"更严重的失败。
				if m.Height <= 0 {
					t.Errorf("Height = %d，期望 > 0", m.Height)
				}

				// 三族都必须能推进 ASCII：拉丁族的主业、CJK 族的内置拉丁、
				// 代码块里的等宽拉丁，缺一个都会让正文变成方框。
				if adv, ok := face.GlyphAdvance('A'); !ok || adv <= 0 {
					t.Errorf("'A' advance = %d, ok = %v，期望 ok 且 > 0", adv, ok)
				}

				// 汉字只要求"量得出推进量"：拉丁族与等宽族没有 CJK 字形，
				// opentype 会回退到 .notdef，所以这里断言的是不 panic 且有尺寸，
				// 真正的覆盖度由 TestSCFamilyCoversGB2312 负责。
				if adv, _ := face.GlyphAdvance('中'); adv <= 0 {
					t.Errorf("'中' advance = %d，期望 > 0", adv)
				}
			})
		}
	}
}

// TestSCFamilyCoversChineseSamples 断言 CJK 族覆盖常用汉字与中文标点，并抽查
// 一批 GB2312 F0–F7 区尾部、语料里几乎不出现的生僻字——子集若是按词频表而不是
// 按 GB2312 来的，这些字会先变成方框。
//
// 全量 7445 个码位的穷举断言在生成期做（tools/fonts/gen_fonts.py --check），
// 那里能直接读 cmap；这里只做冒烟，避免在测试里维护一张 6763 字的表。
func TestSCFamilyCoversChineseSamples(t *testing.T) {
	set, err := LoadFonts()
	if err != nil {
		t.Fatalf("LoadFonts: %v", err)
	}

	samples := []rune(
		"中华人民共和国渲染字体测试表格代码块引用列表标题段落" +
			"，。、；：？！“”‘’（）《》〈〉「」『』【】…—·～" +
			"髯鬈鬟鬣麂麈麒麝麟黛黝黠黟黩黯鼬鼯齄",
	)
	for _, w := range []Weight{WeightRegular, WeightBold} {
		face := faceOf(t, set, FontSC, w)
		for _, r := range samples {
			if _, ok := face.GlyphAdvance(r); !ok {
				t.Errorf("%s 缺少字形 %q (U+%04X)", w, r, r)
			}
		}
	}
}

// TestFontsAreEmbeddedNotReadFromDisk 断言六个字体随二进制分发：把工作目录
// 换成空临时目录后再解析，任何相对路径读取都会在这里失败。
func TestFontsAreEmbeddedNotReadFromDisk(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	set, err := LoadFonts()
	if err != nil {
		t.Fatalf("在工作目录 %q 下 LoadFonts: %v（字体必须来自 go:embed）", dir, err)
	}
	if _, err := set.Font(FontSans, WeightRegular); err != nil {
		t.Fatalf("Font: %v", err)
	}

	// embed.FS 是编译期产物：条目数不符说明有人改回了 os.ReadFile 或漏打包文件。
	entries, err := fontFiles.ReadDir("fonts")
	if err != nil {
		t.Fatalf("读取内嵌目录: %v", err)
	}
	var ttf int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".ttf" {
			ttf++
		}
	}
	if ttf != 6 {
		t.Errorf("内嵌 .ttf 数量 = %d，期望 6", ttf)
	}
}

// TestUnknownFamilyIsAnError 保证拼错族名时得到错误而不是 nil 字体（nil 会在
// NewFace 里变成更难定位的 panic）。
func TestUnknownFamilyIsAnError(t *testing.T) {
	set, err := LoadFonts()
	if err != nil {
		t.Fatalf("LoadFonts: %v", err)
	}
	if _, err := set.Font(Family("comic-sans"), WeightRegular); err == nil {
		t.Fatal("未知字体族应返回错误")
	}
	if _, err := set.Font(FontSans, Weight(500)); err == nil {
		t.Fatal("未知字重应返回错误")
	}
}

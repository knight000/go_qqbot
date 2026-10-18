// Package render 把受支持的 markdown 画成 PNG。
//
// 本文件只管字体资源：三族 × 两字重的**静态实例**用 go:embed 打进二进制，
// 在启动期一次解析成 *opentype.Font。运行期不读系统字体、不写临时文件——
// 「Windows 开发机与 Debian 生产机出图一致」的前提取决于这一点。
//
// 字体各自覆盖的范围（决定渲染器怎么回退）：
//
//	FontSans  拉丁、希腊、西里尔；没有 CJK 字形
//	FontSC    GB2312 全部 7445 个双字节码位（含 6763 个汉字）+ ASCII + 常用标点
//	FontMono  ASCII、拉丁扩展与框线字符，用于代码块
//
// 字体由 tools/fonts/gen_fonts.py 生成，来源与许可证见 fonts/OFL.txt。
package render

import (
	"embed"
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/image/font/opentype"
)

// Family 是一族字体的标识。
type Family string

const (
	// FontSans 是正文字体 Noto Sans（拉丁）。中文由 FontSC 承接。
	FontSans Family = "sans"
	// FontSC 是 CJK 字体 Noto Sans SC，按 GB2312 子集化。
	FontSC Family = "sc"
	// FontMono 是等宽字体 Noto Sans Mono，用于代码块与 ASCII 表格。
	FontMono Family = "mono"
)

// Weight 是字重。数值即 OpenType 的 usWeightClass，只有这两档是内嵌的。
type Weight int

const (
	// WeightRegular 对应 Regular 400，也是 v1 处理斜体的方式（无 italic 字面）。
	WeightRegular Weight = 400
	// WeightBold 对应 Bold 700。
	WeightBold Weight = 700
)

func (w Weight) String() string {
	switch w {
	case WeightRegular:
		return "regular"
	case WeightBold:
		return "bold"
	}
	return strconv.Itoa(int(w))
}

// fontFiles 是唯一的字体来源。embed 的目录必须与 .go 文件在同一包目录下，
// 所以 ttf 就放在 internal/render/fonts/。
//
//go:embed fonts/*.ttf
var fontFiles embed.FS

type faceKey struct {
	family Family
	weight Weight
}

// fontFileOf 把族/字重映射到内嵌文件。写成显式表而不是拼字符串：文件名里
// 不出现族名的拼写变体（Sans vs SC），拼错会变成运行期才发现的缺文件。
var fontFileOf = map[faceKey]string{
	{FontSans, WeightRegular}: "fonts/NotoSans-400.ttf",
	{FontSans, WeightBold}:    "fonts/NotoSans-700.ttf",
	{FontSC, WeightRegular}:   "fonts/NotoSansSC-400.ttf",
	{FontSC, WeightBold}:      "fonts/NotoSansSC-700.ttf",
	{FontMono, WeightRegular}: "fonts/NotoSansMono-400.ttf",
	{FontMono, WeightBold}:    "fonts/NotoSansMono-700.ttf",
}

// FontSet 是已解析的六份字体。零值不可用，只由 LoadFonts 构造。
//
// 解析（约 300 ms，全部花在 20 MB 字体的字节码扫描上）放在启动期，
// 代价换成第一次渲染不必凭空多付这段时间。
type FontSet struct {
	fonts map[faceKey]*opentype.Font
}

// LoadFonts 解析全部六份内嵌字体。
//
// 失败只可能来自二进制本身——字体是编译进来的，不存在"服务器没装字体"这种
// 运行时状况，所以调用方（cmd/bot）应当把错误当作启动失败，而不是降级。
func LoadFonts() (*FontSet, error) {
	set := &FontSet{fonts: make(map[faceKey]*opentype.Font, len(fontFileOf))}
	for k, name := range fontFileOf {
		data, err := fontFiles.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("render: 读取内嵌字体 %s: %w", name, err)
		}
		f, err := opentype.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("render: 解析内嵌字体 %s: %w", name, err)
		}
		set.fonts[k] = f
	}
	return set, nil
}

// Font 返回可直接交给 opentype.NewFace 的字体。
//
// 不缓存 face：字号属于布局器，一个文件会被切成十几个字号，缓存留在调用方。
func (s *FontSet) Font(f Family, w Weight) (*opentype.Font, error) {
	if s == nil {
		return nil, errors.New("render: 字体集未初始化")
	}
	if fnt, ok := s.fonts[faceKey{f, w}]; ok {
		return fnt, nil
	}
	return nil, fmt.Errorf("render: 未内嵌的字体 %s/%s", f, w)
}

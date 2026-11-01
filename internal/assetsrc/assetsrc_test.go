package assetsrc

import (
	"strings"
	"testing"
)

// 预设表里的每一项都必须真的能拼出一个地址：加一项的前置条件是"实测 200"，
// 而这条用例保证的只是"形状对"——缺 {asset} 或占位符写错会让整档失效。
func TestPresetsAreUsable(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Presets() {
		if p.Name == "" || p.Verified == "" {
			t.Errorf("预设缺少名字或实测日期: %+v", p)
		}
		if seen[p.Name] {
			t.Errorf("预设名重复: %q", p.Name)
		}
		seen[p.Name] = true
		if err := ValidateTemplate(p.Template); err != nil {
			t.Errorf("预设 %s 的模板不合法: %v", p.Name, err)
		}
		if !strings.Contains(p.Template, placeholderAsset) {
			t.Errorf("预设 %s 的模板缺 {asset}: %s", p.Name, p.Template)
		}
	}

	// 默认预设必须在表里：留空取它，取不到就等于"默认配错"。
	if _, ok := presetByName(DefaultMirror); !ok {
		t.Errorf("默认预设 %q 不在预设表里（可选：%s）", DefaultMirror, strings.Join(PresetNames(), " / "))
	}

	// 返回的是副本：调用方改不动包内的表。
	got := Presets()
	got[0].Template = "tampered"
	if Presets()[0].Template == "tampered" {
		t.Error("Presets 返回了内部切片，调用方能改到预设表")
	}
}

// 三档来源解析出的模板与拼出的完整地址（#70 验收 1 的单元层）。
func TestResolveAssetSourceTiers(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		mirror  string
		asset   string // asset_url
		base    string // asset_base
		wantSrc string
		wantPre string
		wantURL string
	}{
		{
			name:    "official（默认）",
			wantSrc: SourceOfficial,
			wantURL: "https://github.com/o/r/releases/download/v1.2.3/a",
		},
		{
			name:    "mirror 留空取默认预设",
			source:  "mirror",
			wantSrc: SourceMirror,
			wantPre: DefaultMirror,
			wantURL: "https://gh-proxy.com/https://github.com/o/r/releases/download/v1.2.3/a",
		},
		{
			name:    "mirror 显式预设",
			source:  "mirror",
			mirror:  "ghproxy-net",
			wantSrc: SourceMirror,
			wantPre: "ghproxy-net",
			wantURL: "https://ghproxy.net/https://github.com/o/r/releases/download/v1.2.3/a",
		},
		{
			name:    "custom 模板",
			source:  "custom",
			asset:   "https://cache.example/gh/{owner}/{repo}/{tag}/{asset}",
			wantSrc: SourceCustom,
			wantURL: "https://cache.example/gh/o/r/v1.2.3/a",
		},
		{
			name:    "custom 沿用老写法 asset_base",
			source:  "custom",
			base:    "https://mirror.example",
			wantSrc: SourceCustom,
			wantURL: "https://mirror.example/o/r/releases/download/v1.2.3/a",
		},
		{
			name:    "没写 source 但写了 asset_url（按自定义走）",
			asset:   "https://cache.example/{asset}",
			wantSrc: SourceCustom,
			wantURL: "https://cache.example/a",
		},
		{
			name:    "没写 source 但写了 asset_base（老写法）",
			base:    "https://mirror.example",
			wantSrc: SourceCustom,
			wantURL: "https://mirror.example/o/r/releases/download/v1.2.3/a",
		},
		{
			name:    "老写法等于官方地址时归到 official",
			base:    "https://github.com",
			wantSrc: SourceOfficial,
			wantURL: "https://github.com/o/r/releases/download/v1.2.3/a",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, err := Resolve(tc.source, tc.mirror, tc.asset, tc.base)
			if err != nil {
				t.Fatalf("ResolveAssetSource: %v", err)
			}
			if src.Source != tc.wantSrc || src.Preset != tc.wantPre {
				t.Errorf("source/preset = %q/%q want %q/%q", src.Source, src.Preset, tc.wantSrc, tc.wantPre)
			}
			if got := src.Render("o", "r", "v1.2.3", "a"); got != tc.wantURL {
				t.Errorf("拼出的地址 = %q\nwant %q", got, tc.wantURL)
			}
		})
	}
}

// 配错一律返回错误、**绝不回落官方**：回落会让"以为走了镜像、实际在直连"
// 只在升级慢到撞 15 分钟上限时暴露。
func TestResolveAssetSourceRejectsMisconfiguration(t *testing.T) {
	cases := []struct {
		name   string
		source string
		mirror string
		asset  string
		want   string
	}{
		{"source 非法", "gitee", "", "", "official / mirror / custom"},
		{"mirror 未知名", "mirror", "ghfast", "", "gh-proxy / ghproxy-net"},
		{"custom 没有模板", "custom", "", "", "但没有模板"},
		{"模板缺 {asset}", "custom", "", "https://cache.example/{owner}/{repo}/{tag}", "缺 {asset}"},
		{"模板含未知占位符", "custom", "", "https://cache.example/{tags}/{asset}", "未知占位符 {tags}"},
		{"模板不是绝对地址", "custom", "", "cache.example/{asset}", "不是 http(s) 绝对地址"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(tc.source, tc.mirror, tc.asset, "")
			if err == nil {
				t.Fatal("配错必须报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误文案缺少 %q：%v", tc.want, err)
			}
		})
	}
}

// 通道说明与"token 覆盖 source"的那句必须稳定：启动日志与 /upgrade 预览
// 共用它，两处说法不一致等于让人再猜一次。
func TestChannelText(t *testing.T) {
	official := AssetSource{Source: SourceOfficial, Template: DefaultTemplate}
	mirror := AssetSource{Source: SourceMirror, Preset: DefaultMirror, Template: Presets()[0].Template}

	if got := ChannelText(official, ""); got != "source=official" {
		t.Errorf("官方通道文案 = %q", got)
	}
	if got := ChannelText(mirror, ""); got != "source=mirror preset=gh-proxy" {
		t.Errorf("镜像通道文案 = %q", got)
	}
	// token 非空时 source 被忽略，原因必须写出来。
	got := ChannelText(mirror, "github_pat_x")
	if !strings.Contains(got, "source=api") || !strings.Contains(got, "token") {
		t.Errorf("有 token 时应说明 source 被忽略: %q", got)
	}
}

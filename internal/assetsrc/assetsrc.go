// Package assetsrc 是自更新"二进制通道从哪取"的全部规则（#70 / ADR-0006 决策 3、12）：
// 三档来源、镜像预设表、URL 模板的校验与拼装。
//
// 为什么单独成包而不是留在 internal/upgrade：配置层要校验模板（配错一律启动
// 失败），而 internal/config 是几乎所有包的叶子——让它 import 一个带 net/http、
// os/exec 的功能包，等于把升级链路的整棵依赖树挂到配置上。这里只依赖标准库
// 的字符串与 url 解析，谁 import 都不带来额外传递依赖。
//
// 与安全边界的关系（三条都不许因为本包放宽）：
//   - index.json 与 checksums.txt 永远走 api_base，与这里的模板无关；
//   - 模板只影响产物地址，校验仍是"只比 SHA-256、值来自官方通道"；
//   - token 非空时根本不用模板（三份产物全走 api_base 的资产端点）。
package assetsrc

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// 三档来源。值就是配置里写的字符串，校验与日志文案共用这一份。
const (
	SourceOfficial = "official"
	SourceMirror   = "mirror"
	SourceCustom   = "custom"
)

// DefaultMirror 是 source = "mirror" 但没写 mirror 时的预设。
//
// 留空取默认而不是报错：`source = "mirror"` 单独一行就该能跑起来，这是
// "切换只改一行配置"的最小形态。
const DefaultMirror = "gh-proxy"

// 模板占位符。四个都是拼一个产物地址所需的表达能力，缺 {asset} 就会永远
// 指向同一个名字（#70 的验收之一）。
const (
	placeholderOwner = "{owner}"
	placeholderRepo  = "{repo}"
	placeholderTag   = "{tag}"
	placeholderAsset = "{asset}"
)

// DefaultTemplate 是官方通道的模板。
//
// 它与 asset_base 时代拼出来的地址**逐字相同**（asset_base = https://github.com
// 时）：`{asset_base}/{owner}/{repo}/releases/download/{tag}/{asset}`。所以
// `source = "official"`、不写 [upgrade] 段、老写法 asset_base = "https://github.com"
// 三者产出同一个 URL，升级路径上没有"新老行为不同"这一说。
const DefaultTemplate = "https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}"

// LegacyTemplate 把老写法 asset_base 翻成等价的模板。
//
// asset_base 退为兼容别名之后仍要能表达"任何前缀"：它当年就是一个前缀，
// 今天只是被翻译成模板的第一个片段。尾部的 "/" 必须吃掉，否则拼出 "//"。
func LegacyTemplate(assetBase string) string {
	return strings.TrimRight(strings.TrimSpace(assetBase), "/") +
		"/{owner}/{repo}/releases/download/{tag}/{asset}"
}

// Preset 是一个镜像预设。
type Preset struct {
	// Name 是配置里写的值（[upgrade].mirror）。
	Name string
	// Template 是完整 URL 模板，支持 {owner} {repo} {tag} {asset}。
	Template string
	// Verified 是最近一次实测可用的日期。预设表不探测、不竞速、不监控
	// （#70 非目标）：镜像失效就改配置，这一栏只回答"上次验证是哪天"。
	Verified string
}

// presets 是有序的预设表。加一项的前置条件是**真的拼出过 200**：
// 主流镜像要的是把整个 https://github.com 塞进路径，只填域名是 404
// （2026-11-01 实测，见 #70 正文）。
var presets = []Preset{
	{
		Name:     "gh-proxy",
		Template: "https://gh-proxy.com/https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}",
		Verified: "2026-11-01",
	},
	{
		Name:     "ghproxy-net",
		Template: "https://ghproxy.net/https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}",
		Verified: "2026-11-01",
	},
}

// Presets 返回预设表的副本（顺序稳定：文档与报错文案都按它排）。
func Presets() []Preset {
	return append([]Preset(nil), presets...)
}

// PresetNames 是"可用项"那一行：配错时必须能直接抄一个走。
func PresetNames() []string {
	out := make([]string, 0, len(presets))
	for _, p := range presets {
		out = append(out, p.Name)
	}
	return out
}

func presetByName(name string) (Preset, bool) {
	for _, p := range presets {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Preset{}, false
}

// AssetSource 是解析后的二进制通道：哪一档、哪个预设、最终模板。
//
// 零值等价于官方通道（Describe 会说 source=official，Render 用默认模板）：
// 一份没有 [upgrade] 段的旧配置走的就是这条路。
type AssetSource struct {
	Source   string // official | mirror | custom
	Preset   string // 仅 source = mirror 时非空
	Template string // 完整 URL 模板
}

// Describe 是"当前走哪条通道"的一句话，启动日志与 /upgrade 预览共用。
//
// 两边必须说同一句话：翻配置文件才能回答"我到底走没走镜像"正是本票要消除的
// 那件事（#70「可见性」）。
func (s AssetSource) Describe() string {
	switch s.Source {
	case SourceMirror:
		return "source=mirror preset=" + s.Preset
	case SourceCustom:
		return "source=custom asset_url=" + s.Template
	default:
		return "source=official"
	}
}

// ChannelText 是那句通道说明，含 token 对通道的覆盖。
//
// token 非空时 source 被**忽略**（私有仓的资产在镜像上匿名取不到，凭据也
// 不能发给用户可配置的主机）。忽略这件事必须在启动日志里写明原因：否则
// 配了镜像却看不到镜像生效，只会被读成"镜像配错了"。
func ChannelText(src AssetSource, token string) string {
	if strings.TrimSpace(token) != "" {
		return "source=api（配置了 token：source 被忽略，三份产物全走 api_base）"
	}
	return src.Describe()
}

// Render 按模板拼出完整地址。owner/repo 分开传是因为上游插件的产物属于
// **另一个仓**（#53），而模板是全局的一份。
func (s AssetSource) Render(owner, repo, tag, asset string) string {
	return Render(s.Template, owner, repo, tag, asset)
}

// Render 按模板拼出完整地址，是唯一的拼装实现。
//
// 空模板按官方通道处理：零值 AssetSource 就是"没配置"，而"没配置"的语义
// 只能是官方通道——回落成空地址会让升级在下载阶段才失败。
func Render(tmpl, owner, repo, tag, asset string) string {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = DefaultTemplate
	}
	return strings.NewReplacer(
		placeholderOwner, owner,
		placeholderRepo, repo,
		placeholderTag, tag,
		placeholderAsset, asset,
	).Replace(tmpl)
}

// Resolve 把 [upgrade] 的四个字段解析成"哪一档 + 最终模板"。
//
// 五类配错里有四类在这里判（第五类 source 非法也在），且**一律报错、绝不
// 回落官方**：用户以为走了镜像、实际在直连，是最坏的失败方式——它只在
// 升级慢到撞上限时暴露，而那时已经浪费了十五分钟。
//
// 没写 source 时按写了的键推断（asset_url / asset_base / mirror 都能独立
// 表达"我要一条非官方通道"）。推断而不是静默忽略，是因为"写了 mirror 却
// 没生效"与本票要消灭的那类意外完全同形。
func Resolve(source, mirror, assetURL, assetBase string) (AssetSource, error) {
	src := strings.ToLower(strings.TrimSpace(source))
	mirror = strings.TrimSpace(mirror)
	assetURL = strings.TrimSpace(assetURL)
	assetBase = strings.TrimSpace(assetBase)
	if src == "" {
		switch {
		case assetURL != "":
			src = SourceCustom
		case assetBase != "":
			// 老写法是 #70 之前唯一存在的形态，必须原样继续工作；而
			// `asset_base = "https://github.com"`（示例配置里原来就有这一行）
			// 拼出来就是官方通道，把它认成 custom 只会让日志显示一个不存在的
			// "自定义通道"，所以这一种值直接归到 official。
			src = SourceCustom
			if LegacyTemplate(assetBase) == DefaultTemplate {
				src = SourceOfficial
			}
		case mirror != "":
			src = SourceMirror
		default:
			src = SourceOfficial
		}
	}

	switch src {
	case SourceOfficial:
		return AssetSource{Source: SourceOfficial, Template: DefaultTemplate}, nil

	case SourceMirror:
		name := strings.ToLower(mirror)
		if name == "" {
			name = DefaultMirror
		}
		p, ok := presetByName(name)
		if !ok {
			return AssetSource{}, fmt.Errorf(
				"[upgrade].mirror=%q 不是已知预设（可选：%s）", mirror, strings.Join(PresetNames(), " / "))
		}
		return AssetSource{Source: SourceMirror, Preset: p.Name, Template: p.Template}, nil

	case SourceCustom:
		tmpl := assetURL
		if tmpl == "" {
			if assetBase == "" {
				return AssetSource{}, errors.New(
					`[upgrade].source = "custom" 但没有模板：写 asset_url` +
						`（支持 {owner} {repo} {tag} {asset}），或沿用老写法 asset_base`)
			}
			tmpl = LegacyTemplate(assetBase)
		}
		if err := ValidateTemplate(tmpl); err != nil {
			return AssetSource{}, err
		}
		return AssetSource{Source: SourceCustom, Template: tmpl}, nil

	default:
		return AssetSource{}, fmt.Errorf("[upgrade].source=%q 非法（可选 %s / %s / %s）",
			strings.TrimSpace(source), SourceOfficial, SourceMirror, SourceCustom)
	}
}

// ValidateTemplate 校验一个二进制通道模板。
//
// 三条规则各自对应一种"能启动、但永远下不下来"的配置：不是 http(s) 绝对
// 地址、缺 {asset}、含未知占位符（手误 `{tags}` 会静默变成一个坏 URL）。
// 报错文案里带上模板原文：配错的人要能一眼看出是哪一段。
func ValidateTemplate(tmpl string) error {
	tmpl = strings.TrimSpace(tmpl)
	if tmpl == "" {
		return errors.New("二进制通道模板为空")
	}
	u, err := url.Parse(tmpl)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("二进制通道模板 %q 不是 http(s) 绝对地址：拼出来的地址取不到产物", tmpl)
	}
	seen := make(map[string]bool, 4)
	for _, ph := range templatePlaceholders(tmpl) {
		switch ph {
		case placeholderOwner, placeholderRepo, placeholderTag, placeholderAsset:
			seen[ph] = true
		default:
			return fmt.Errorf("二进制通道模板 %q 含未知占位符 %s（只认 {owner} {repo} {tag} {asset}）",
				tmpl, ph)
		}
	}
	if !seen[placeholderAsset] {
		return fmt.Errorf("二进制通道模板 %q 缺 {asset}：拼出来的地址永远指向同一个名字，取不到产物", tmpl)
	}
	return nil
}

// templatePlaceholders 扫出模板里的 {…} 片段（含花括号，便于原样进错误文案）。
//
// 不用正则：这里的语法只有"左花括号到右花括号"，一个手写扫描比一条正则更
// 容易看懂边界（比如 `{` 后面没有 `}` 时不是占位符，是普通字符）。
func templatePlaceholders(tmpl string) []string {
	var out []string
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '{' {
			continue
		}
		rest := tmpl[i:]
		j := strings.IndexByte(rest, '}')
		if j < 0 {
			break
		}
		out = append(out, rest[:j+1])
		i += j
	}
	return out
}

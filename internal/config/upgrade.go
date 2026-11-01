package config

import (
	"fmt"
	"strings"

	// 通道的解析与校验在 assetsrc（一个只依赖标准库的叶子包），不在这里：
	// 配置层是几乎所有包的叶子，让它 import 带 net/http / os/exec 的升级链路
	// 等于把整棵依赖树挂到每个读配置的地方。
	"github.com/knight000/go_qqbot/internal/assetsrc"
)

// Upgrade 是自更新配置（ADR-0006 决策 2/3/12）。
//
// 两个通道分开不是可选项：「校验和与二进制不同通道」是结构约束，不是约定。
// index.json 与 checksums.txt 永远从 api_base 取；二进制走 source 选出的通道
// （官方 / 镜像预设 / 自定义模板，#70），设了 token 之后三份产物全走 api_base
// ——私有仓的资产无法在镜像上匿名取，而 token 绝不能发给用户可配置的主机。
//
// 四个字段（source / mirror / asset_url / asset_base）在 applyDefaults 里被
// 解析成**一个**模板（assetsrc.AssetSource）：三档来源在实现上只有模板不同，
// 多一条"拼装路径"就多一处能配错的地方。
type Upgrade struct {
	// Enabled 是总开关，默认 true。关掉之后连检查都不发请求，
	// 不是"检查了但不下载"。
	Enabled *bool `toml:"enabled"`
	// APIBase 是元数据通道（GitHub API 端点），默认 https://api.github.com。
	APIBase string `toml:"api_base"`
	// Source 是二进制通道：official（默认）| mirror | custom。
	Source string `toml:"source"`
	// Mirror 是 source = "mirror" 时的预设名；留空取 gh-proxy。
	Mirror string `toml:"mirror"`
	// AssetURL 是 source = "custom" 时的完整模板，支持
	// {owner} {repo} {tag} {asset} 四个占位符，{asset} 必填。
	AssetURL string `toml:"asset_url"`
	// AssetBase 是 #70 之前的写法，退为兼容别名：等价于 source = "custom"
	// 且模板为 "{asset_base}/{owner}/{repo}/releases/download/{tag}/{asset}"。
	// 保留它只为让老配置拼出的地址**逐字不变**。
	AssetBase string `toml:"asset_base"`
	// Token 是私有仓凭据（细粒度 token，只需 contents:read）。
	// 它在文件里就是明文——与 [[bots]].secret 是同一类东西，配置本身要 600。
	Token string `toml:"token"`

	// assetSource 是上面四个字段解析出的最终通道，由 applyDefaults 填。
	// 不是 TOML 字段：它的值只由解析决定，写进配置文件没有意义。
	assetSource assetsrc.AssetSource
}

// UpgradeEnabled 返回自更新开关的最终值（默认 true）。
func (c *Config) UpgradeEnabled() bool {
	if c.Upgrade.Enabled == nil {
		return true
	}
	return *c.Upgrade.Enabled
}

// AssetSource 返回解析后的二进制通道（哪一档、哪个预设、最终模板）。
//
// 启动日志与 /upgrade 预览都读它：翻配置文件才能回答"我到底走没走镜像"，
// 正是 #70 要消除的那件事。
func (u Upgrade) AssetSource() assetsrc.AssetSource { return u.assetSource }

// resolveAssetSource 是 applyDefaults 与 Validate 共用的那一次解析。
//
// 同一个纯函数调两次而不是把结果藏进字段：解析失败时 Validate 要能原样报出
// 错误，而"藏起来的半成品 + 一个 error 字段"只会让状态多一种可能。
func (u Upgrade) resolveAssetSource() (assetsrc.AssetSource, error) {
	return assetsrc.Resolve(u.Source, u.Mirror, u.AssetURL, u.AssetBase)
}

// applyUpgradeSource 把四个字段归一、解析成模板，并收集"写了但不生效"的提示。
//
// 解析失败不在这里报错（applyDefaults 没有错误出口）：Validate 会用同一个
// 函数再算一遍并逐条报出来，配错一律启动失败。
func (c *Config) applyUpgradeSource() {
	c.Upgrade.Source = strings.ToLower(strings.TrimSpace(c.Upgrade.Source))
	c.Upgrade.Mirror = strings.ToLower(strings.TrimSpace(c.Upgrade.Mirror))
	c.Upgrade.AssetURL = strings.TrimSpace(c.Upgrade.AssetURL)
	c.Upgrade.AssetBase = strings.TrimSpace(c.Upgrade.AssetBase)
	// 显式写了 source 才谈得上"另外几个键被它盖住"：没写 source 的老配置
	// 里 asset_base 不是被忽略，而是它就是通道本身。
	explicit := c.Upgrade.Source != ""

	src, err := c.Upgrade.resolveAssetSource()
	if err != nil {
		c.Upgrade.assetSource = assetsrc.AssetSource{}
		return
	}
	c.Upgrade.assetSource = src
	// 刻意**不**把解析出的档位写回 Source：写回会让这个函数不再幂等（第二次
	// 进来时 Source 已经有值，老写法 asset_base 会被误判成"被 official 盖住"），
	// 而"现在走哪一档"由 AssetSource() 回答，不需要第二份事实。
	c.Warnings = append(c.Warnings, c.upgradeSourceWarnings(src, explicit)...)
}

// upgradeSourceWarnings 报"写了但被忽略"的字段。
//
// 这些不是错误（配置能启动、通道也确定），但静默忽略正是本票要消灭的那类
// 意外：配了镜像却没生效，只会在升级慢到撞上限时暴露。
func (c *Config) upgradeSourceWarnings(src assetsrc.AssetSource, explicit bool) []string {
	dead := func(names ...string) string { return strings.Join(names, " / ") }

	switch src.Source {
	case assetsrc.SourceOfficial:
		if !explicit {
			return nil
		}
		var ignored []string
		if c.Upgrade.Mirror != "" {
			ignored = append(ignored, "mirror")
		}
		if c.Upgrade.AssetURL != "" {
			ignored = append(ignored, "asset_url")
		}
		if c.Upgrade.AssetBase != "" {
			ignored = append(ignored, "asset_base")
		}
		if len(ignored) > 0 {
			return []string{fmt.Sprintf(
				"[upgrade].source = %q 时 %s 不生效：二进制走官方直连（要镜像就写 source = \"mirror\"）",
				assetsrc.SourceOfficial, dead(ignored...))}
		}

	case assetsrc.SourceMirror:
		var ignored []string
		if c.Upgrade.AssetURL != "" {
			ignored = append(ignored, "asset_url")
		}
		if c.Upgrade.AssetBase != "" {
			ignored = append(ignored, "asset_base")
		}
		if len(ignored) > 0 {
			return []string{fmt.Sprintf(
				"[upgrade].source = %q 时 %s 不生效：地址由预设 %s 给出（要自定义就改成 source = \"custom\"）",
				assetsrc.SourceMirror, dead(ignored...), src.Preset)}
		}

	case assetsrc.SourceCustom:
		if c.Upgrade.AssetURL != "" && c.Upgrade.AssetBase != "" {
			return []string{"[upgrade] 同时写了 asset_url 与 asset_base：以 asset_url 为准" +
				"（asset_base 是老写法，只在没有 asset_url 时生效）"}
		}
	}
	return nil
}

// upgradeSourceProblems 是 Validate 里的"配错立刻报错"。
//
// 五类都走 assetsrc.Resolve 一份实现：source 非法 / mirror 未知名 /
// custom 没有模板 / 模板缺 {asset} / 模板含未知占位符。报错文案自带可用项，
// 照抄一个就能改对。
func (c *Config) upgradeSourceProblems() []string {
	if _, err := c.Upgrade.resolveAssetSource(); err != nil {
		return []string{err.Error()}
	}
	return nil
}

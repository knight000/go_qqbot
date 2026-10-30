// Package config 负责加载与校验全局配置。
//
// 设计要点（对应 ARCHITECTURE.md §9.4、ADR-0008 决策 10）：
//   - 配置分两层：进程级（[core] 与其余顶层段）与每 Bot 业务面。本包只管进程级，
//     每个 Bot 的业务配置在它自己的库里；
//   - 一个进程托管多个 Bot：[[bots]] 一 Bot 一条，全是进程级配置的一部分，
//     因为 Core 启动前就必须知道有哪些 Bot；
//   - 旧形态的单个 [app] 段仍可用，Load 会把它翻译成 Bots[0]（见 normalize）；
//   - 同一份代码、同一套配置结构，沙箱与正式只差 [core].sandbox；
//   - 认证信息写在本地 config 文件里（config.local.toml，不入 git），
//     同时保留 QQ_APP_ID / QQ_APP_SECRET 环境变量覆盖，便于生产环境收紧权限。
//     环境变量只有单值：唯一一个 Bot 时照旧生效，多个 Bot 时只覆盖第一个并
//     告警——多值环境变量没有可用的表达形式。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration 让 TOML 里可以直接写 "5s" / "168h"。
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.Duration.String()), nil }

type Config struct {
	// Core 是进程级配置：谁是 Core 所有者，以及这个进程连的是哪套平台端点。
	Core Core `toml:"core"`
	// Bots 是 [[bots]] 数组，一个平台 Bot 一条。顺序是事实——启动错峰、
	// 日志与错误定位都按它来，所以是有序切片而不是 map。
	Bots []Bot `toml:"bots"`
	// App 是旧形态的单 Bot 段，只为"老配置不改也能起"存在：Load 会把它
	// 翻译成 Bots[0] 与 Core 的平台端点，之后运行期一律看 Bots。
	App        LegacyApp  `toml:"app"`
	Bot        BotOptions `toml:"bot"`
	Store      Store      `toml:"store"`
	Retention  Retention  `toml:"retention"`
	Media      Media      `toml:"media"`
	HTTP       HTTP       `toml:"http"`
	Active     Active     `toml:"active"`
	Permission Permission `toml:"permission"`
	Group      Group      `toml:"group"`
	Log        Log        `toml:"log"`
	Debug      Debug      `toml:"debug"`
	Plugins    Plugins    `toml:"plugins"`
	Upgrade    Upgrade    `toml:"upgrade"`
	// Warnings 是 Load 收集的"这份配置能用，但有一处写法必须让人知道"的提示。
	// 启动期原样打 warn（见 cmd/bot/main.go）。它不进 TOML、不参与校验：
	// 一旦变成错误，用户会在升级机器人的路上被一条风格问题拦住。
	Warnings []string `toml:"-"`
}

// Upgrade 是自更新配置（ADR-0006 决策 2/3）。
//
// 两个地址分开不是可选项：「校验和与二进制不同通道」是结构约束，不是约定。
// index.json 与 checksums.txt 永远从 api_base 取；二进制默认从 asset_base 取
// （可指向镜像），设了 token 之后三份产物全走 api_base——私有仓的资产无法在
// 镜像上匿名取，而 token 绝不能发给用户可配置的镜像主机。
type Upgrade struct {
	// Enabled 是总开关，默认 true。关掉之后连检查都不发请求，
	// 不是"检查了但不下载"。
	Enabled *bool `toml:"enabled"`
	// APIBase 是元数据通道（GitHub API 端点），默认 https://api.github.com。
	APIBase string `toml:"api_base"`
	// AssetBase 是二进制通道，默认 https://github.com；可指向镜像。
	// 只在没有 token 时生效。
	AssetBase string `toml:"asset_base"`
	// Token 是私有仓凭据（细粒度 token，只需 contents:read）。
	// 它在文件里就是明文——与 [[bots]].secret 是同一类东西，配置本身要 600。
	Token string `toml:"token"`
}

// UpgradeEnabled 返回自更新开关的最终值（默认 true）。
func (c *Config) UpgradeEnabled() bool {
	if c.Upgrade.Enabled == nil {
		return true
	}
	return *c.Upgrade.Enabled
}

// Plugins 是 `[plugins]` 表：既有核心侧的停用名单，也有各插件的配置段。
//
// 为什么是一个带 UnmarshalTOML 的类型，而不是 `map[string]map[string]any`
// 再加一个顶层字段（ADR-0005 决策 13）：TOML 里 `disabled` 与 `[plugins.echo]`
// 处在**同一个表**中，一个表不能同时解进两个字段。而 `disabled = ["echo"]`
// 是数组、配置段是表——按同一类型解会直接报「期望表、实际数组」，整个启动
// 读不到配置。所以这里按 TOML 的类型分流：键名 disabled 走数组，其余一律
// 当配置段，且必须是表（写错成 `[plugins] echo = 1` 要立刻报错，而不是被
// 静默丢掉）。
//
// 两者的语义刻意不同（决策 13）：disabled 是"装而不跑"，由核心决定是否
// spawn；`[plugins.<name>].enabled` 仍是插件自己的功能开关。
type Plugins struct {
	// Disabled 是核心侧停用名单，里面的名字一律不 spawn。
	Disabled []string
	// Sections 是 `[plugins.<name>]` 的原始配置，键是插件名。
	Sections map[string]map[string]any
}

// UnmarshalTOML 把 `[plugins]` 的一张表拆成 Disabled 与 Sections。
func (p *Plugins) UnmarshalTOML(v any) error {
	tbl, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("[plugins] 必须是表，实际 %T", v)
	}
	for key, raw := range tbl {
		if key == "disabled" {
			names, err := toStrings("[plugins].disabled", raw)
			if err != nil {
				return err
			}
			p.Disabled = names
			continue
		}
		section, ok := raw.(map[string]any)
		if !ok {
			// 最可能的现场是把 `disabled` 拼错（`dissabled = [...]`）：把它当
			// 插件配置段丢掉，症状会是"停用没生效"而不是"配置写错了"。
			return fmt.Errorf("[plugins].%s 必须是表（插件配置段）；[plugins] 下只有 disabled 是数组，实际 %T", key, raw)
		}
		if p.Sections == nil {
			p.Sections = make(map[string]map[string]any, len(tbl))
		}
		p.Sections[key] = section
	}
	return nil
}

// toStrings 把 TOML 数组收敛成 []string，顺带给出能指到键名的错误。
func toStrings(label string, raw any) ([]string, error) {
	switch arr := raw.(type) {
	case []string:
		return arr, nil
	case []any:
		out := make([]string, 0, len(arr))
		for i, item := range arr {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s[%d] 必须是字符串，实际 %T", label, i, item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s 必须是字符串数组，实际 %T", label, raw)
	}
}

// Core 是进程级配置（`[core]`）。
//
// 它是"进程"这一层的唯一入口：Core 所有者管的是进程（/reload /restart
// /upgrade /reset all /bot 系列），与某个 Bot 自己库里的 admins 表无关——
// 后者只够管那一个 Bot。不分区的话，任何一个租户都能重启整台机器上所有人的
// Bot（ADR-0008 决策 9）。
type Core struct {
	// Owners 是 Core 所有者的 openid 列表。它是进程级命令的唯一授权来源；
	// 本版本只解析出来给装配层，消费在 T4。
	Owners []string `toml:"owners"`
	// Sandbox 为 true 时连沙箱域名。用指针区分"没写"与"显式 false"：
	// 旧形态的 [app].sandbox 要能填进来，而显式写 false 也不能被旧值盖掉。
	Sandbox *bool `toml:"sandbox"`
	// APIBase / TokenURL 是平台端点。进程级：同一台机器上的所有 Bot 连的是
	// 同一套平台环境，端点没有"每 Bot 一份"的语义。留空按 sandbox 推导。
	// 旧形态的 [app].api_base / [app].token_url 会落到这里。
	APIBase  string `toml:"api_base"`
	TokenURL string `toml:"token_url"`
}

// 认证类型（GLOSSARY「认证类型」，ADR-0008 决策 6）。
//
// 它决定这个 Bot **能触及谁**，不决定消息配额：未认证只能被所有者私聊，
// 个人认证只能进自己是群主/管理员的群与开发体验用户，企业认证可公开加群。
// 三条限制由平台在推送前执行——不在范围内的会话，事件根本到不了核心，
// 所以核心不持有来源白名单，这里只做登记与合法性校验。
const (
	CertUnverified = "unverified"
	CertPersonal   = "personal"
	CertEnterprise = "enterprise"
)

// DefaultQuotaProfile 是配额档名的缺省值。配额表在 T7 落地，本版本只承载档名。
const DefaultQuotaProfile = "default"

// Bot 是 `[[bots]]` 的一项：一个平台 Bot 的凭据与身份声明。
//
// 为什么不把业务面（启用插件、人设、开关默认值）写在这里：那些东西该由租户
// 自己改，而配置文件只有 Core 所有者能改。业务面存在该 Bot 自己的库里
// （ADR-0008 决策 10）。
type Bot struct {
	// AppID 是平台侧机器人 ID，也是数据目录名与单实例锁的维度。
	AppID string `toml:"appid"`
	// Secret 是 AppSecret。它在文件里就是明文——与旧的 [app].secret 同一类
	// 东西，配置本身要 600。任何日志、轨迹、错误串都不许把它写出来。
	Secret string `toml:"secret"`
	// Enabled 缺省 true；显式 false 表示这个 Bot 不连接（状态 suspended）。
	// 指针只为区分"没写"与"显式 false"：缺省值必须是启用。
	Enabled *bool `toml:"enabled"`
	// CertType 取 unverified | personal | enterprise，缺省 personal。
	CertType string `toml:"cert_type"`
	// QuotaProfile 指向全局配额表，缺省 default。
	QuotaProfile string `toml:"quota_profile"`
	// DataDir 覆盖数据目录的推导结果；留空即 <datadir>/bots/<appid>。
	DataDir string `toml:"data_dir"`
	// legacy 标记这一项来自旧的 [app] 段，只影响错误信息怎么给它指路。
	// 解码器会跳过未导出字段，所以它不会出现在 TOML 里。
	legacy bool
}

// IsEnabled 返回这个 Bot 是否启用（缺省 true）。
func (b Bot) IsEnabled() bool { return b.Enabled == nil || *b.Enabled }

// SandboxEnabled 返回沙箱开关的最终值（默认 false，即正式环境）。
func (c *Config) SandboxEnabled() bool {
	if c.Core.Sandbox == nil {
		return false
	}
	return *c.Core.Sandbox
}

// BotDataDir 返回这个 Bot 的数据目录：<datadir>/bots/<appid>，可用 data_dir 覆盖。
//
// datadir 是 [store].path 所在的那一层（路径本身是文件），与启动期的口径
// 逐字一致。数据目录按 appid 推导而不是按顺序编号：运维看到 appid 就知道
// 该备份哪一目录，去掉一个 Bot 也不会让别人的目录整体挪位。
func (c *Config) BotDataDir(b Bot) string {
	if dir := strings.TrimSpace(b.DataDir); dir != "" {
		return dir
	}
	return filepath.Join(filepath.Dir(c.Store.Path), "bots", b.AppID)
}

// BotStorePath 返回这个 Bot 的 SQLite 路径：<BotDataDir>/qqbot.db。
//
// 一 Bot 一库（ADR-0008 决策 1）：库文件落在该 Bot 自己的数据目录里，
// 备份、迁移、清理某一户时不会碰到别人。启动期开库、单实例锁与启动日志
// 都必须用这一处推导，否则"日志说在 A、实际打开 B"这类问题无从排查。
func (c *Config) BotStorePath(b Bot) string {
	return filepath.Join(c.BotDataDir(b), "qqbot.db")
}

// LegacyApp 是旧形态的单个 `[app]` 段。
//
// 它只在 Load 期间被翻译成 Bots[0] 与 [core] 的平台端点，翻译之后运行期不再
// 读它。留着它是因为 `[[bots]]` 与 `[app]` 必须有一个能通过解码：老配置
// 不改也要能起（#61 验收），这是升级路径上最容易踩空的一步。
type LegacyApp struct {
	ID       string `toml:"id"`
	Secret   string `toml:"secret"`
	Sandbox  bool   `toml:"sandbox"`
	APIBase  string `toml:"api_base"`
	TokenURL string `toml:"token_url"`
}

// BotOptions 是 `[bot]` 段：网关连接参数。
//
// 它至今仍是进程级的一份（全部 Bot 共用），与 `[[bots]]` 只差一个 s——这两个
// 名字挨得太近，注释在这里写明：`[bot]` 是连接参数，`[[bots]]` 是 Bot 列表。
// 每 Bot 独立的连接参数（intents 与分片）属于后续票的事，本包不动。
type BotOptions struct {
	Intents    uint64 `toml:"intents"`
	ShardTotal int    `toml:"shard_total"`
	ShardID    int    `toml:"shard_id"`
	Resume     bool   `toml:"resume"`
	// GatewayStagger 是相邻两个 Bot 取 GET /gateway/bot 之间的最小间隔。
	//
	// 该接口限 2 QPM，而"按 AppID 还是按 IP 计量"平台没有说明。按 IP 的话
	// N 个 Bot 同时启动会互相挤配额，所以默认串行并留 30s 间隔。
	// 用指针区分"没写"（默认 30s）与显式 `0s`（不错峰，只在确认按 AppID
	// 计量之后才该这么配）。
	GatewayStagger *Duration `toml:"gateway_stagger"`
}

// DefaultGatewayStagger 是启动错峰的默认间隔（对齐 GET /gateway 的 2 QPM）。
const DefaultGatewayStagger = 30 * time.Second

// GatewayStaggerInterval 返回启动错峰的实际间隔；0 表示不错峰。
func (o BotOptions) GatewayStaggerInterval() time.Duration {
	if o.GatewayStagger == nil {
		return DefaultGatewayStagger
	}
	return o.GatewayStagger.Duration
}

// Store 是数据目录的定位锚点。
//
// 一 Bot 一库之后 Path 指向的文件本身不再被打开：它的**目录**是 datadir
// （升级标记、插件产物都在这一层），每个 Bot 的库在
// <datadir>/bots/<appid>/qqbot.db（见 BotStorePath）。保留这个键是为了
// 不破坏既有配置——"数据放哪一层"仍然由它决定。
type Store struct {
	Path string `toml:"path"`
}

type Retention struct {
	Messages Duration `toml:"messages"`
	Events   Duration `toml:"events"`
	JobRuns  Duration `toml:"job_runs"`
}

// Media 是入站媒体下载的配置，**保留但未启用**。
//
// 入站媒体（图片/音视频）一律不下载，只记 has_media 与 msg_type；
// 出站发图走富媒体上传，与这段配置无关。字段留着是为了不再改一次配置
// 结构（以及 /reset 里的媒体文件清理路径），enabled=true 当前无效果。
type Media struct {
	Enabled      bool   `toml:"enabled"`
	Dir          string `toml:"dir"`
	MaxFileBytes int64  `toml:"max_file_bytes"`
	QuotaBytes   int64  `toml:"quota_bytes"`
}

type HTTP struct {
	Timeout    Duration `toml:"timeout"`
	Retries    int      `toml:"retries"`
	RatePerMin int      `toml:"rate_per_min"`
	// Domains 为特定主机名配置独立额度（每分钟请求数），形如
	//   [http.domains]
	//   "api.torn.com" = 800
	// 未列出的域名走 RatePerMin。配了独立额度的域名只使用该桶（不叠加）。
	// 零或负数在启动期被忽略并告警，回退默认值——这里不做校验，
	// 否则一次笔误会让机器人起不来。
	Domains map[string]int `toml:"domains"`
}

// Active 是主动消息策略。Enabled 用指针区分"未配置"与"显式关闭"：
// 未配置时按开启处理（启动通知依赖它），显式写 false 才真正关掉。
type Active struct {
	Enabled      *bool    `toml:"enabled"`
	Plugins      []string `toml:"plugins"`
	DailyPerPeer int      `toml:"daily_per_peer"`
}

// ActiveEnabled 返回主动消息总开关的最终值。
func (c *Config) ActiveEnabled() bool {
	if c.Active.Enabled == nil {
		return true
	}
	return *c.Active.Enabled
}

// Permission 是全局权限策略。
//
// 权限本身由核心在路由阶段判定（见 ARCHITECTURE.md §8.7），这里只控制
// "拒绝时的表现"：插件通过 DenyReply 提供文案，是否真的发出去由这个开关决定。
type Permission struct {
	// DenyReply 控制权限不足时是否回复插件声明的拒绝语。
	// 默认 false（静默）：在群里回一句"你没权限"等于告诉所有人这里有条隐藏指令，
	// 还会暴露机器人的能力边界。私聊或调试期可以临时打开。
	DenyReply bool `toml:"deny_reply"`
}

// Group 是群准入闸门（ARCHITECTURE.md §8.8）。
//
// 白名单与黑名单是两套独立开关，默认都开启：
//   - 白名单开启时，只有被显式加入白名单的群能进入机器人；
//   - 黑名单开启时，被加入黑名单的群一律丢弃（优先级高于白名单）；
//   - 两个都关闭时，任何群消息都放行。
//
// 用指针是为了区分"没写"与"显式关闭"：默认值必须是 true，否则升级后
// 白名单是空的，机器人会把所有群都当成未授权。
type Group struct {
	Whitelist *bool `toml:"whitelist"`
	Blacklist *bool `toml:"blacklist"`
	// FetchName 控制首次见到新群时是否调用 GET /v2/groups/{id}/info 取群名。
	// 该接口目前仅白名单机器人可用（错误码 11253），取不到就留空，
	// 之后可用 /group name 手动设置。默认开启：能取到时省一次手工操作。
	FetchName *bool `toml:"fetch_name"`
	// Notify 控制"入群/退群"这类群生命周期通知私聊发给谁：
	// owner（默认，只发给所有者）| admin（所有者 + 管理员）| off（不发）。
	// 默认只发所有者，因为新群是否放行是所有权者一个人的决定，
	// 把每条入群通知抄送给所有管理员只会制造噪音。
	Notify string `toml:"notify"`
}

// GroupNotifyMode 归一化 [group].notify。
//
// 写错的值一律回退 owner 而不是 off：通知发多了只是吵，发少了会让
// "机器人被拉进某个群"这件本该由人决定的事悄无声息地过去。
func (c *Config) GroupNotifyMode() string {
	switch strings.ToLower(strings.TrimSpace(c.Group.Notify)) {
	case "admin", "admins":
		return NotifyAdmin
	case "off", "none", "false", "0":
		return NotifyOff
	default:
		return NotifyOwner
	}
}

// 群生命周期通知的收件人范围。
const (
	NotifyOwner = "owner"
	NotifyAdmin = "admin"
	NotifyOff   = "off"
)

// WhitelistEnabled 返回白名单开关的最终值（默认 true）。
func (c *Config) WhitelistEnabled() bool {
	if c.Group.Whitelist == nil {
		return true
	}
	return *c.Group.Whitelist
}

// BlacklistEnabled 返回黑名单开关的最终值（默认 true）。
func (c *Config) BlacklistEnabled() bool {
	if c.Group.Blacklist == nil {
		return true
	}
	return *c.Group.Blacklist
}

// GroupNameFetchEnabled 返回"是否自动取群名"的最终值（默认 true）。
func (c *Config) GroupNameFetchEnabled() bool {
	if c.Group.FetchName == nil {
		return true
	}
	return *c.Group.FetchName
}

type Log struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

// Debug 是调试模式：把收到的事件、命中的插件与回复内容打印到控制台。
// 与 log.level 分开——日志给机器检索，调试轨迹给人看一条完整链路。
// 命令行 -debug 可临时打开（不改配置文件）。
// 调试轨迹级别。
const (
	// DebugFull 打印收到的每条事件、命中插件、发送结果与未命中提示。
	DebugFull = "full"
	// DebugPlugin 只打印触发插件的内容与真正发出的回复正文，
	// 不含事件 ID、union 与无关消息。
	DebugPlugin = "plugin"
)

type Debug struct {
	Enabled bool `toml:"enabled"`
	// Level 是轨迹详细程度：full（默认，收到/命中/发送全链路）或
	// plugin（只打印触发插件的内容与回复正文）。
	Level string `toml:"level"`
	// Raw 额外打印平台原始 JSON，排查"某字段平台到底给没给"时用。
	// 只在 level=full 时生效。
	Raw bool `toml:"raw"`
	// UnknownEvents 让未知事件（解码表里还没有的平台事件）附带原始 JSON，
	// 用来确认平台新推了什么。未知事件永不进入插件路由。
	UnknownEvents bool `toml:"unknown_events"`
}

// Plugin 返回某个插件的配置段（不存在时返回空 map，插件自行取默认值）。
func (c *Config) Plugin(name string) map[string]any {
	if c.Plugins.Sections == nil {
		return map[string]any{}
	}
	return c.Plugins.Sections[name]
}

// PluginDisabled 报告某个插件是否在 `[plugins].disabled` 里（核心侧停用）。
//
// 判定按名字精确匹配：插件名来自 manifest、大小写敏感，配置里写错大小写
// 应当是"没停用"而不是"猜着停用"。
func (c *Config) PluginDisabled(name string) bool {
	for _, n := range c.Plugins.Disabled {
		if n == name {
			return true
		}
	}
	return false
}

// Load 读取配置、把旧形态归一、套默认值、做环境变量覆盖，最后校验。
func Load(path string) (*Config, error) {
	c := new(Config)
	if err := decodeAndNormalize(path, c); err != nil {
		return nil, err
	}
	c.applyEnv()
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadForInit 读取配置、把旧形态归一、套默认值，但**不校验**。
//
// `qqbot init --plugins` 只需要 [store].path 来定位数据目录。init 生成的
// 配置里 appid/secret 本来就是留空的，用 Load 会因为"还没填 Bot 凭据"而
// 拒绝释放插件——而那正是这个命令要救的现场（ADR-0005 决策 12）。
func LoadForInit(path string) (*Config, error) {
	c := new(Config)
	if err := decodeAndNormalize(path, c); err != nil {
		return nil, err
	}
	c.applyDefaults()
	return c, nil
}

// decodeAndNormalize 读文件、解 TOML、把旧形态归一，两个入口共用。
//
// 归一化必须走同一条路径：漏了它，一份老配置在 Load 里是"一个 Bot"，
// 在 LoadForInit 里却是"没有 Bot"，init 与启动就会落到不同的数据目录。
func decodeAndNormalize(path string, c *Config) error {
	md, err := decodeFile(path, c)
	if err != nil {
		return err
	}
	if err := c.normalize(md); err != nil {
		return fmt.Errorf("解析配置 %s: %w", path, err)
	}
	return nil
}

// normalize 把旧形态的 [app] 翻译成 Bots[0]，并拒绝两种形态同时出现。
//
// 判定用 toml.MetaData.IsDefined 而不是"App 的字段是否非空"：一份只写了
// `[app]`、id 留空的配置（init 刚生成的样子）也是旧形态，不能因为值是空串
// 就当成"没有 [app] 段"。
func (c *Config) normalize(md toml.MetaData) error {
	if !md.IsDefined("app") {
		return nil
	}
	if len(c.Bots) > 0 {
		// 两个来源都在，静默择一是最坏的处置：用户会以为自己登记的那个 Bot
		// 生效了，实际上跑到另一个 Bot 上。二义一律拒绝，并在同一句话里
		// 说清迁移动作——转成 [[bots]] 时 [app] 的端点没有等价位置，
		// 不提醒就会被静默丢掉、直接连上正式环境。
		return errors.New("[app] 与 [[bots]] 不能同时出现：前者是单 Bot 的旧形态，" +
			"后者是数组形态，两处都写等于两个来源（留 [app] 就删掉 [[bots]]；" +
			"反过来转成 [[bots]] 时，把 [app] 的 sandbox / api_base / token_url 搬到 [core]）")
	}
	c.Bots = []Bot{{
		AppID:  c.App.ID,
		Secret: c.App.Secret,
		legacy: true,
	}}
	// 平台端点是进程级的，旧形态把它们塞在 [app] 里，这里搬到 [core]。
	// 显式写了 [core] 的以 [core] 为准——新形态的人不该被兼容路径盖住。
	if c.Core.Sandbox == nil {
		sandbox := c.App.Sandbox
		c.Core.Sandbox = &sandbox
	}
	if c.Core.APIBase == "" {
		c.Core.APIBase = c.App.APIBase
	}
	if c.Core.TokenURL == "" {
		c.Core.TokenURL = c.App.TokenURL
	}
	return nil
}

// decodeFile 读取并解析配置文件。
//
// 缺文件单独给一句话：那是新用户看到的第一条错误，必须直接指向
// `qqbot init`，而不是 "open xxx: The system cannot find the file specified"。
func decodeFile(path string, c *Config) (toml.MetaData, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return toml.MetaData{}, fmt.Errorf("配置文件 %s 不存在：运行 `qqbot init -c %s` 生成一份", path, path)
	}
	if err != nil {
		return toml.MetaData{}, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	md, err := toml.Decode(string(data), c)
	if err != nil {
		return toml.MetaData{}, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	return md, nil
}

// applyEnv 用环境变量覆盖凭据。
//
// 只有"恰好一个 Bot"的形态能表达：QQ_APP_ID / QQ_APP_SECRET 是单值，
// 多值环境变量没有可用的表达形式。多 Bot 时只覆盖第一个并记一条 warn——
// 静默只改一个比不报更危险，用户会以为所有 Bot 都换成了环境变量里的凭据。
// 仍然覆盖第一个而不是整体忽略：容器里"只换主机、凭据走环境"是既有用法，
// 一律不认会让升级直接起不来。
func (c *Config) applyEnv() {
	id := strings.TrimSpace(os.Getenv("QQ_APP_ID"))
	secret := strings.TrimSpace(os.Getenv("QQ_APP_SECRET"))
	if id == "" && secret == "" {
		return
	}
	if len(c.Bots) == 0 {
		// 一份只写了 [store] 的骨架配置 + 环境变量凭据，是既有用法
		// （旧形态的空 [app] 段等价于此），不能因为改成数组就断掉。
		c.Bots = []Bot{{AppID: id, Secret: secret}}
		return
	}
	if id != "" {
		c.Bots[0].AppID = id
	}
	if secret != "" {
		c.Bots[0].Secret = secret
	}
	if len(c.Bots) > 1 {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"QQ_APP_ID/QQ_APP_SECRET 只覆盖了 %s：多值环境变量没有可用的表达形式，"+
				"宁可吵不要静默（其余 %d 个 Bot 沿用配置文件里的凭据）",
			botLabel(0, c.Bots[0]), len(c.Bots)-1))
	}
}

func (c *Config) applyDefaults() {
	// 每个 Bot 的缺省值。cert_type 缺省 personal：旧形态没有这个概念，
	// 而 personal 正是"自己的 Bot 自己用"的档位；quota_profile 缺省 default
	// 指向配额表的默认档（表在 T7 落，这里只承载档名）。
	//
	// appid/secret/data_dir 在这里 Trim：环境变量早就 Trim 过，文件里手抄的值
	// 却可能带首尾空格，而 appid 会被拼进数据目录与锁文件名、secret 会进
	// Authorization——带着空格只会在很远的地方炸，校验时 Trim 而不写回等于
	// 把问题留到运行时。
	for i := range c.Bots {
		b := &c.Bots[i]
		b.AppID = strings.TrimSpace(b.AppID)
		b.Secret = strings.TrimSpace(b.Secret)
		b.DataDir = strings.TrimSpace(b.DataDir)
		if b.CertType = strings.ToLower(strings.TrimSpace(b.CertType)); b.CertType == "" {
			b.CertType = CertPersonal
		}
		if b.QuotaProfile = strings.TrimSpace(b.QuotaProfile); b.QuotaProfile == "" {
			b.QuotaProfile = DefaultQuotaProfile
		}
	}
	// Core 所有者的 openid 只做 Trim 与去空：空串永远匹配不上任何发送者
	// （平台的 openid 非空），留在列表里只会让"我明明写了 owner"更难排查。
	if len(c.Core.Owners) > 0 {
		owners := make([]string, 0, len(c.Core.Owners))
		for _, o := range c.Core.Owners {
			if o = strings.TrimSpace(o); o != "" {
				owners = append(owners, o)
			}
		}
		c.Core.Owners = owners
	}
	if c.Core.APIBase == "" {
		if c.SandboxEnabled() {
			c.Core.APIBase = "https://sandbox.api.sgroup.qq.com"
		} else {
			c.Core.APIBase = "https://api.sgroup.qq.com"
		}
	}
	if c.Core.TokenURL == "" {
		c.Core.TokenURL = "https://bots.qq.com/app/getAppAccessToken"
	}
	if c.Bot.Intents == 0 {
		c.Bot.Intents = 1 << 25 // GROUP_AND_C2C_EVENT
	}
	if c.Bot.ShardTotal <= 0 {
		c.Bot.ShardTotal = 1
	}
	if c.Store.Path == "" {
		c.Store.Path = "./data/qqbot.db"
	}
	if c.Media.Dir == "" {
		c.Media.Dir = "./data/media"
	}
	if c.Media.MaxFileBytes <= 0 {
		c.Media.MaxFileBytes = 8 << 20
	}
	if c.Media.QuotaBytes <= 0 {
		c.Media.QuotaBytes = 256 << 20
	}
	if c.Retention.Messages.Duration <= 0 {
		c.Retention.Messages.Duration = 168 * time.Hour
	}
	if c.Retention.Events.Duration <= 0 {
		c.Retention.Events.Duration = 72 * time.Hour
	}
	if c.Retention.JobRuns.Duration <= 0 {
		c.Retention.JobRuns.Duration = 168 * time.Hour
	}
	if c.HTTP.Timeout.Duration <= 0 {
		c.HTTP.Timeout.Duration = 5 * time.Second
	}
	if c.HTTP.RatePerMin <= 0 {
		c.HTTP.RatePerMin = 100
	}
	if len(c.Active.Plugins) == 0 {
		c.Active.Plugins = []string{"bootnotice"}
	}
	if c.Active.DailyPerPeer <= 0 {
		c.Active.DailyPerPeer = 5
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Debug.Level == "" {
		c.Debug.Level = DebugFull
	}
	c.Debug.Level = strings.ToLower(strings.TrimSpace(c.Debug.Level))
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	// 自更新两个地址的默认值必须在这里（而不是空串让 upgrade 包兜）：
	// /ping 与启动日志里要能看到实际生效的地址，空白的配置项看不出用的是哪一个。
	if c.Upgrade.APIBase == "" {
		c.Upgrade.APIBase = "https://api.github.com"
	}
	if c.Upgrade.AssetBase == "" {
		c.Upgrade.AssetBase = "https://github.com"
	}
}

func (c *Config) Validate() error {
	var problems []string
	// appid 为空是新用户的第一面（init 生成的配置里它留空），所以错过的提示
	// 必须同时给出两条出路：跑 init 生成配置，或按字段名直接填。
	const appidHint = "运行 `qqbot init` 生成配置并填 [app].id（旧形态）或 [[bots]].appid" +
		"（QQ 开放平台 → 机器人 → 开发设置）"
	if len(c.Bots) == 0 {
		problems = append(problems, "没有登记任何 Bot："+appidHint)
	}
	seen := make(map[string]int, len(c.Bots))
	for i, b := range c.Bots {
		label := botLabel(i, b)
		switch appid := strings.TrimSpace(b.AppID); {
		case appid == "":
			problems = append(problems, label+".appid 为空："+appidHint)
		case !validSlug(appid):
			problems = append(problems, fmt.Sprintf(
				"%s.appid=%q 含非法字符：数据目录按 appid 推导，只允许字母、数字、下划线与连字符",
				label, b.AppID))
		default:
			if prev, ok := seen[appid]; ok {
				problems = append(problems, fmt.Sprintf(
					"%s 与 bots[%d] 的 appid 重复：同一个 Bot 只能登记一次", label, prev))
			} else {
				seen[appid] = i
			}
		}
		if strings.TrimSpace(b.Secret) == "" {
			// 与 appid 不同，这里不提示 init：配置已经在手上了，再跑一次 init 也
			// 只会写出同一个空 secret，真正缺的是密钥本身。
			problems = append(problems, label+".secret 为空（写入本地 config，勿提交 git）")
		}
		certType := b.CertType
		if certType == "" {
			certType = CertPersonal
		}
		switch certType {
		case CertUnverified, CertPersonal, CertEnterprise:
		default:
			problems = append(problems, fmt.Sprintf("%s.cert_type=%q 非法（可选 %s / %s / %s）",
				label, b.CertType, CertUnverified, CertPersonal, CertEnterprise))
		}
		if p := b.QuotaProfile; p != "" && !validSlug(p) {
			problems = append(problems, fmt.Sprintf(
				"%s.quota_profile=%q 非法（只允许字母、数字、下划线与连字符）", label, b.QuotaProfile))
		}
	}
	if c.Bot.ShardID < 0 || c.Bot.ShardID >= c.Bot.ShardTotal {
		problems = append(problems, fmt.Sprintf("[bot].shard_id=%d 超出 [0,%d)", c.Bot.ShardID, c.Bot.ShardTotal))
	}
	// 负间隔没有意义：串行与间隔只能加长启动，不能倒着来。显式 0 合法
	// （关掉错峰），所以要按指针判而不是按值判。
	if c.Bot.GatewayStagger != nil && c.Bot.GatewayStagger.Duration < 0 {
		problems = append(problems, fmt.Sprintf(
			"[bot].gateway_stagger=%s 不能为负（写 \"0s\" 表示不错峰）", c.Bot.GatewayStagger.Duration))
	}
	if strings.TrimSpace(c.Store.Path) == "" {
		problems = append(problems, "store.path 为空")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("log.level=%q 非法", c.Log.Level))
	}
	switch strings.ToLower(c.Log.Format) {
	case "text", "json":
	default:
		problems = append(problems, fmt.Sprintf("log.format=%q 非法", c.Log.Format))
	}
	switch strings.ToLower(c.Debug.Level) {
	case DebugFull, DebugPlugin:
	default:
		problems = append(problems, fmt.Sprintf("debug.level=%q 非法（可选 full / plugin）", c.Debug.Level))
	}
	if c.Debug.Raw && strings.EqualFold(c.Debug.Level, DebugPlugin) {
		problems = append(problems, "debug.raw 需要 level=full：plugin 级别不输出原始事件")
	}
	if len(problems) > 0 {
		return fmt.Errorf("配置校验失败:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// botLabel 给校验错误定位到某一个 Bot：优先 appid（人能对上平台后台），
// 空 appid 时退到数组下标；来自旧 [app] 段的再注明出处，免得用户在
// 一份没有 [[bots]] 的配置里找不到被点名的 "bots[0]" 是哪一段。
func botLabel(i int, b Bot) string {
	parts := make([]string, 0, 2)
	if id := strings.TrimSpace(b.AppID); id != "" {
		parts = append(parts, "appid="+id)
	}
	if b.legacy {
		parts = append(parts, "来自 [app] 段")
	}
	if len(parts) == 0 {
		return fmt.Sprintf("bots[%d]", i)
	}
	return fmt.Sprintf("bots[%d]（%s）", i, strings.Join(parts, "，"))
}

// validSlug 判断一个值能否当目录名与配置键：字母、数字、下划线与连字符。
//
// appid 会被直接拼进数据目录（<datadir>/bots/<appid>），放行 "." 或路径
// 分隔符等于让一条配置把库写到数据目录之外；配额档名同理，它将来是
// 配额表里的键。
func validSlug(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '-':
		default:
			return false
		}
	}
	return s != ""
}

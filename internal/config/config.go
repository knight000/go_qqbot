// Package config 负责加载与校验全局配置。
//
// 设计要点（对应 ARCHITECTURE.md §9.4）：
//   - 同一份代码、同一套配置结构，沙箱与正式只差 sandbox 开关；
//   - 认证信息写在本地 config 文件里（config.local.toml，不入 git），
//     同时保留 QQ_APP_ID / QQ_APP_SECRET 环境变量覆盖，便于生产环境收紧权限。
package config

import (
	"fmt"
	"os"
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
	App        App                       `toml:"app"`
	Bot        Bot                       `toml:"bot"`
	Store      Store                     `toml:"store"`
	Retention  Retention                 `toml:"retention"`
	Media      Media                     `toml:"media"`
	HTTP       HTTP                      `toml:"http"`
	Active     Active                    `toml:"active"`
	Permission Permission                `toml:"permission"`
	Group      Group                     `toml:"group"`
	Log        Log                       `toml:"log"`
	Debug      Debug                     `toml:"debug"`
	Plugins    map[string]map[string]any `toml:"plugins"`
}

type App struct {
	ID       string `toml:"id"`
	Secret   string `toml:"secret"`
	Sandbox  bool   `toml:"sandbox"`
	APIBase  string `toml:"api_base"`
	TokenURL string `toml:"token_url"`
}

type Bot struct {
	Intents    uint64 `toml:"intents"`
	ShardTotal int    `toml:"shard_total"`
	ShardID    int    `toml:"shard_id"`
	Resume     bool   `toml:"resume"`
}

type Store struct {
	Path string `toml:"path"`
}

type Retention struct {
	Messages Duration `toml:"messages"`
	Events   Duration `toml:"events"`
	JobRuns  Duration `toml:"job_runs"`
}

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
	if c.Plugins == nil {
		return map[string]any{}
	}
	return c.Plugins[name]
}

// Load 读取配置、套默认值、做环境变量覆盖，最后校验。
func Load(path string) (*Config, error) {
	c := new(Config)
	if _, err := toml.DecodeFile(path, c); err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	c.applyEnv()
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("QQ_APP_ID"); v != "" {
		c.App.ID = v
	}
	if v := os.Getenv("QQ_APP_SECRET"); v != "" {
		c.App.Secret = v
	}
}

func (c *Config) applyDefaults() {
	if c.App.APIBase == "" {
		if c.App.Sandbox {
			c.App.APIBase = "https://sandbox.api.sgroup.qq.com"
		} else {
			c.App.APIBase = "https://api.sgroup.qq.com"
		}
	}
	if c.App.TokenURL == "" {
		c.App.TokenURL = "https://bots.qq.com/app/getAppAccessToken"
	}
	if c.Bot.Intents == 0 {
		c.Bot.Intents = 1 << 25 // GROUP_AND_C2C_EVENT
	}
	if c.Bot.ShardTotal <= 0 {
		c.Bot.ShardTotal = 1
	}
	if c.Store.Path == "" {
		c.Store.Path = "./.temp/data/qqbot.db"
	}
	if c.Media.Dir == "" {
		c.Media.Dir = "./.temp/media"
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
}

func (c *Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.App.ID) == "" {
		problems = append(problems, "app.id 为空（QQ 开放平台机器人 AppID）")
	}
	if strings.TrimSpace(c.App.Secret) == "" {
		problems = append(problems, "app.secret 为空（写入本地 config，勿提交 git）")
	}
	if c.Bot.ShardID < 0 || c.Bot.ShardID >= c.Bot.ShardTotal {
		problems = append(problems, fmt.Sprintf("bot.shard_id=%d 超出 [0,%d)", c.Bot.ShardID, c.Bot.ShardTotal))
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

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
	App       App                       `toml:"app"`
	Bot       Bot                       `toml:"bot"`
	Store     Store                     `toml:"store"`
	Retention Retention                 `toml:"retention"`
	Media     Media                     `toml:"media"`
	HTTP      HTTP                      `toml:"http"`
	Log       Log                       `toml:"log"`
	Plugins   map[string]map[string]any `toml:"plugins"`
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

type Log struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
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
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
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
	if len(problems) > 0 {
		return fmt.Errorf("配置校验失败:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

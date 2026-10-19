package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	return path
}

// 未配置 [active] 时按开启处理：启动通知依赖它，显式 false 才关闭。
func TestActiveDefaultsToEnabled(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ActiveEnabled() {
		t.Fatal("未配置 [active] 时应默认开启主动消息")
	}
	if len(cfg.Active.Plugins) != 1 || cfg.Active.Plugins[0] != "bootnotice" {
		t.Fatalf("默认白名单应只有 bootnotice，实际 %v", cfg.Active.Plugins)
	}
	if cfg.Active.DailyPerPeer != 5 {
		t.Fatalf("默认日预算应为 5，实际 %d", cfg.Active.DailyPerPeer)
	}
}

func TestActiveCanBeDisabledExplicitly(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"

[active]
enabled = false
plugins = ["bootnotice", "other"]
daily_per_peer = 2
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveEnabled() {
		t.Fatal("显式 enabled=false 应关闭")
	}
	if len(cfg.Active.Plugins) != 2 || cfg.Active.DailyPerPeer != 2 {
		t.Fatalf("显式配置未生效: %+v", cfg.Active)
	}
}

// 调试模式必须默认关闭：生产环境不能默认把消息正文打到控制台。
func TestDebugDefaultsOff(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Debug.Enabled || cfg.Debug.Raw {
		t.Fatalf("调试模式应默认关闭，实际 %+v", cfg.Debug)
	}
	if cfg.Debug.Level != DebugFull {
		t.Fatalf("debug.level 未写入时应默认 full，实际 %q", cfg.Debug.Level)
	}
}

func TestDebugParsed(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"

[debug]
enabled = true
raw = true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Debug.Enabled || !cfg.Debug.Raw {
		t.Fatalf("调试配置未生效: %+v", cfg.Debug)
	}
	// 不写 level 时沿用 full，保证旧配置文件行为不变
	if cfg.Debug.Level != DebugFull {
		t.Fatalf("debug.level 默认应为 full，实际 %q", cfg.Debug.Level)
	}
}

func TestDebugLevelPlugin(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"

[debug]
enabled = true
level = "PLUGIN"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Debug.Level != DebugPlugin {
		t.Fatalf("level 应归一为 %q，实际 %q", DebugPlugin, cfg.Debug.Level)
	}
}

func TestDebugLevelInvalid(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"

[debug]
level = "verbose"
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "debug.level") {
		t.Fatalf("非法 level 应报错，实际 err=%v", err)
	}
}

// raw 是"原始事件"，与 plugin 级别（只看插件）相冲突，配置时直接拒绝。
func TestDebugRawRequiresFull(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"

[debug]
level = "plugin"
raw = true
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "debug.raw") {
		t.Fatalf("plugin + raw 应报错，实际 err=%v", err)
	}
}

// 群准入两套开关默认都开：未配置时必须按"只允许白名单群"处理。
func TestGroupGateDefaultsToEnabled(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WhitelistEnabled() {
		t.Fatal("白名单应默认开启")
	}
	if !cfg.BlacklistEnabled() {
		t.Fatal("黑名单应默认开启")
	}
	if !cfg.GroupNameFetchEnabled() {
		t.Fatal("自动取群名应默认开启")
	}
}

// 显式关闭必须生效：这是"只开一套名单"或"完全不设准入"的唯一方式。
func TestGroupGateCanBeDisabledExplicitly(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"

[group]
whitelist = false
blacklist = false
fetch_name = false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WhitelistEnabled() || cfg.BlacklistEnabled() || cfg.GroupNameFetchEnabled() {
		t.Fatalf("显式 false 应关闭全部群准入开关: %+v", cfg.Group)
	}
}

// 域名独立额度是通用能力：配置写在这里，未列出的域名继续走默认桶。
// 零/负数不在这里拒绝——由出站层在启动期告警并回退，避免一次笔误让机器人起不来。
func TestHTTPDomainQuotasParsed(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"

[http]
rate_per_min = 100

[http.domains]
"api.torn.com" = 800
"Example.COM" = 50
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.RatePerMin != 100 {
		t.Fatalf("默认额度=%d", cfg.HTTP.RatePerMin)
	}
	if got := cfg.HTTP.Domains["api.torn.com"]; got != 800 {
		t.Fatalf("api.torn.com 额度=%d", got)
	}
	if got := cfg.HTTP.Domains["Example.COM"]; got != 50 {
		t.Fatalf("域名键应原样保留（大小写在出站层归一化），实际 %d", got)
	}
}

// 入群/退群通知默认只发所有者。写错的值回退 owner 而不是 off：
// 通知发多了只是吵，发少了会让"机器人被谁拉进了哪个群"悄无声息地过去。
func TestGroupNotifyMode(t *testing.T) {
	cases := map[string]string{
		"":        NotifyOwner,
		"owner":   NotifyOwner,
		"OWNER":   NotifyOwner,
		" admin ": NotifyAdmin,
		"admins":  NotifyAdmin,
		"off":     NotifyOff,
		"none":    NotifyOff,
		"typo":    NotifyOwner,
	}
	for raw, want := range cases {
		cfg := &Config{}
		cfg.Group.Notify = raw
		if got := cfg.GroupNotifyMode(); got != want {
			t.Errorf("notify=%q 应归一化为 %q，实际 %q", raw, want, got)
		}
	}
}

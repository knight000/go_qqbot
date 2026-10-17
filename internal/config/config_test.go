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

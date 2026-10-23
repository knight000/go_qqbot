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

// 新用户的第一面：配置文件还不存在。错误必须直接给出 `qqbot init`，
// 而不是一条 "open xxx: The system cannot find the file specified"——
// 那既没说缺的是什么，也没说下一步干什么。
func TestMissingConfigPointsToInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.local.toml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("配置不存在时应当报错")
	}
	if !strings.Contains(err.Error(), "qqbot init") {
		t.Fatalf("缺配置的错误要指向 qqbot init，实际 %q", err)
	}
}

// app.id 为空是 `qqbot init` 生成的配置必然撞上的第一条错误（示例里 id
// 就是留空的）。它必须同时给出两条出路：跑 init 生成配置，或直接填 [app].id，
// 而不是淹没在一串通用校验清单里。
func TestEmptyAppIDPointsToInitAndField(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	path := writeConfig(t, `
[app]
id = ""
secret = "s"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("app.id 为空时应当报错")
	}
	for _, want := range []string{"qqbot init", "[app].id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误串 %q 缺少 %q：要能看出填哪个字段或跑哪条命令", err, want)
		}
	}
}

// secret 为空不给 init 提示：配置文件已经在手上，缺的是密钥本身，
// 再跑一次 init 只会得到同一个空 secret。两种"为空"的处置不能混成一条。
func TestEmptySecretDoesNotPointToInit(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	path := writeConfig(t, `
[app]
id = "123"
secret = ""
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("app.secret 为空时应当报错")
	}
	if !strings.Contains(err.Error(), "app.secret") {
		t.Fatalf("错误串应指到 app.secret，实际 %q", err)
	}
	if strings.Contains(err.Error(), "qqbot init") {
		t.Errorf("secret 为空不该把用户推去跑 init，实际 %q", err)
	}
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

// [plugins] 同时装两样东西：核心侧的停用名单（数组）与各插件的配置段（表）。
// 这两者在 TOML 里是同一个表的不同键，所以解码必须区分类型而不是一律当配置段
// ——否则 `disabled = [...]` 会以「期望表、实际数组」把整个配置读死（ADR-0005 决策 13）。
func TestPluginsDisabledCoexistsWithSections(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"

[plugins]
disabled = ["echo", "torn"]

[plugins.demo]
delay = "10s"
size = 3

[plugins.md]
font = "serif"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("disabled 与本段共存时不该失败: %v", err)
	}
	if !cfg.PluginDisabled("echo") || !cfg.PluginDisabled("torn") {
		t.Fatalf("停用名单未生效: %v", cfg.Plugins.Disabled)
	}
	if cfg.PluginDisabled("demo") {
		t.Fatal("demo 不在 disabled 里，不该被判为停用")
	}
	demo := cfg.Plugin("demo")
	if demo["delay"] != "10s" || demo["size"] != int64(3) {
		t.Fatalf("demo 配置段 = %#v", demo)
	}
	// 段名必须按原样保留：插件名取自 manifest，大小写敏感。
	if got := cfg.Plugin("md")["font"]; got != "serif" {
		t.Fatalf("md 配置段 = %#v", cfg.Plugin("md"))
	}
	// 不存在的段是空（长度 0，与 nil map 等价）：插件一律「取默认值」，
	// 不该写判空分支。
	if got := cfg.Plugin("missing"); len(got) != 0 {
		t.Fatalf("不存在的段应返回空 map，实际 %#v", got)
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

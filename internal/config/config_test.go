package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if !strings.Contains(err.Error(), ".secret 为空") {
		t.Fatalf("错误串应指到 secret 为空，实际 %q", err)
	}
	// 旧形态要能被认出来：用户写的明明是 [app]，报错却只说 bots[0] 会让人
	// 在一份没有 [[bots]] 的配置里找不到北。
	if !strings.Contains(err.Error(), "[app] 段") {
		t.Errorf("旧形态的错误应注明来自 [app] 段，实际 %q", err)
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

// 默认数据目录不能叫 .temp：同一个默认值既是开发期的，也是 init 交给发布件
// 用户的唯一建议值。名字里带 temp 的目录会被 git clean、清理脚本、rsync
// --exclude 顺手带走——里面装着数据库、WAL、媒体和全部插件二进制。
func TestDefaultDataDirIsNotTemp(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "123"
secret = "s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Path != "./data/qqbot.db" {
		t.Errorf("[store].path 默认值 = %q，期望 ./data/qqbot.db", cfg.Store.Path)
	}
	if cfg.Media.Dir != "./data/media" {
		t.Errorf("[media].dir 默认值 = %q，期望 ./data/media", cfg.Media.Dir)
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

// 新形态是数组：[core] 管进程，[[bots]] 一 Bot 一条。顺序必须是配置里的顺序
// ——启动错峰、日志与错误定位都按它来。
func TestBotsArrayParses(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	path := writeConfig(t, `
[core]
owners = [" 10001 ", "", "10002"]

[[bots]]
appid = "102000001"
secret = "s1"

[[bots]]
appid = "102000002"
secret = "s2"
enabled = false
cert_type = "ENTERPRISE"
quota_profile = " tight "
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("多 Bot 配置应能加载: %v", err)
	}
	if len(cfg.Bots) != 2 {
		t.Fatalf("应解析出 2 个 Bot，实际 %d: %+v", len(cfg.Bots), cfg.Bots)
	}
	first, second := cfg.Bots[0], cfg.Bots[1]
	if first.AppID != "102000001" || first.Secret != "s1" {
		t.Errorf("第一个 Bot 的凭据不对: %+v", first)
	}
	// 缺省：enabled=true、cert_type=personal、quota_profile=default。
	if !first.IsEnabled() {
		t.Error("enabled 缺省必须是 true")
	}
	if first.CertType != CertPersonal {
		t.Errorf("cert_type 缺省应为 %q，实际 %q", CertPersonal, first.CertType)
	}
	if first.QuotaProfile != DefaultQuotaProfile {
		t.Errorf("quota_profile 缺省应为 %q，实际 %q", DefaultQuotaProfile, first.QuotaProfile)
	}
	if second.IsEnabled() {
		t.Error("显式 enabled=false 应生效")
	}
	// 认证类型与配额档都要归一化（大小写与首尾空格），否则校验会为一个
	// 手写的 "ENTERPRISE" 判非法。
	if second.CertType != CertEnterprise {
		t.Errorf("cert_type 应归一为 %q，实际 %q", CertEnterprise, second.CertType)
	}
	if second.QuotaProfile != "tight" {
		t.Errorf("quota_profile 应去空格，实际 %q", second.QuotaProfile)
	}
	// Core 所有者只做 Trim 与去空：空串永远匹配不上任何 openid。
	if got := strings.Join(cfg.Core.Owners, ","); got != "10001,10002" {
		t.Errorf("owners = %q，期望 10001,10002", got)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("单值配置不该有告警: %v", cfg.Warnings)
	}
}

// 数据目录按 appid 推导：<store.path 所在目录>/bots/<appid>/，可由 data_dir 覆盖。
// 推导而不是编号：运维看到 appid 就知道该备份哪个目录，删掉一个 Bot 也不会让
// 别人的目录整体挪位。
func TestBotDataDirDerivedAndOverridable(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	path := writeConfig(t, `
[store]
path = "./srv/qqbot.db"

[[bots]]
appid = "102000001"
secret = "s"

[[bots]]
appid = "102000002"
secret = "s"
data_dir = "/var/lib/qqbot/manual"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.BotDataDir(cfg.Bots[0]), filepath.Join("srv", "bots", "102000001"); got != want {
		t.Errorf("推导的数据目录 = %q，期望 %q", got, want)
	}
	if got, want := cfg.BotDataDir(cfg.Bots[1]), "/var/lib/qqbot/manual"; got != want {
		t.Errorf("data_dir 覆盖未生效: %q", got)
	}
}

// 一 Bot 一库：库文件必须落在该 Bot 自己的数据目录里，且与日志、单实例锁
// 用同一处推导。两个 appid 的库路径不能相同——那正是物理隔离要保证的事。
func TestBotStorePathFollowsDataDir(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	path := writeConfig(t, `
[store]
path = "./srv/qqbot.db"

[[bots]]
appid = "102000001"
secret = "s"

[[bots]]
appid = "102000002"
secret = "s"
data_dir = "/var/lib/qqbot/manual"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	first, second := cfg.BotStorePath(cfg.Bots[0]), cfg.BotStorePath(cfg.Bots[1])
	if want := filepath.Join("srv", "bots", "102000001", "qqbot.db"); first != want {
		t.Errorf("推导的库路径 = %q，期望 %q", first, want)
	}
	if want := filepath.Join("/var/lib/qqbot/manual", "qqbot.db"); second != want {
		t.Errorf("data_dir 覆盖后的库路径 = %q，期望 %q", second, want)
	}
	if first == second {
		t.Fatalf("两个 Bot 的库路径不能相同: %q", first)
	}
}

// 启动错峰的默认值必须自己生效：写 [[bots]] 的人不该为了"别撞 2 QPM"
// 去翻文档发现还得配一个键。显式 0s 是"不错峰"，与"没写"必须分得开。
func TestGatewayStaggerDefaultsAndCanBeDisabled(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"没写用默认", "", DefaultGatewayStagger},
		{"显式 0s 不错峰", "gateway_stagger = \"0s\"", 0},
		{"可配置", "gateway_stagger = \"2s\"", 2 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeConfig(t, `
[bot]
`+c.body+`

[[bots]]
appid = "102000001"
secret = "s"
`)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("加载配置: %v", err)
			}
			if got := cfg.Bot.GatewayStaggerInterval(); got != c.want {
				t.Fatalf("gateway_stagger 生效值 = %s，期望 %s", got, c.want)
			}
		})
	}
}

// 负间隔是笔误，不是"更快的错峰"：串行加间隔只能变慢。写 0s 才是关掉。
func TestNegativeGatewayStaggerRejected(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	path := writeConfig(t, `
[bot]
gateway_stagger = "-1s"

[[bots]]
appid = "102000001"
secret = "s"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("负的 gateway_stagger 应当被拒绝")
	}
	if !strings.Contains(err.Error(), "gateway_stagger") {
		t.Fatalf("错误应点名 gateway_stagger，实际: %v", err)
	}
}

// 旧配置不改也能起：单个 [app] 段按一个 Bot 解析，认证类型取 personal，
// sandbox / api_base / token_url 这些进程级端点搬到 [core]。
func TestLegacyAppBecomesSingleBot(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	path := writeConfig(t, `
[app]
id = "102000009"
secret = "legacy-secret"
sandbox = true
api_base = "https://example.test/api"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("旧配置应能加载: %v", err)
	}
	if len(cfg.Bots) != 1 {
		t.Fatalf("旧 [app] 应翻译成一个 Bot，实际 %d 个", len(cfg.Bots))
	}
	if b := cfg.Bots[0]; b.AppID != "102000009" || b.Secret != "legacy-secret" {
		t.Errorf("旧凭据未翻译: %+v", b)
	}
	if cfg.Bots[0].CertType != CertPersonal {
		t.Errorf("旧形态的认证类型应为 %q，实际 %q", CertPersonal, cfg.Bots[0].CertType)
	}
	if !cfg.SandboxEnabled() {
		t.Error("旧 [app].sandbox=true 必须继续生效")
	}
	if cfg.Core.APIBase != "https://example.test/api" {
		t.Errorf("旧 [app].api_base 应搬到 [core]，实际 %q", cfg.Core.APIBase)
	}
	if cfg.Core.TokenURL == "" {
		t.Error("token_url 应取到默认值")
	}
}

// 两种形态同时出现是二义：静默择一会让用户以为登记的 Bot 生效了，
// 实际跑到另一个号上。必须拒绝，且说清删哪一边。
func TestAppAndBotsBothPresentRejected(t *testing.T) {
	path := writeConfig(t, `
[app]
id = "102000009"
secret = "s"

[[bots]]
appid = "102000010"
secret = "s"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("[app] 与 [[bots]] 同时出现必须报错")
	}
	for _, want := range []string{"[app]", "[[bots]]", "不能同时出现"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误串 %q 缺少 %q", err, want)
		}
	}
}

// 单 Bot 时环境变量照旧生效，且不该产生任何告警。
func TestEnvOverridesSingleBot(t *testing.T) {
	t.Setenv("QQ_APP_ID", "102999999")
	t.Setenv("QQ_APP_SECRET", "from-env")
	path := writeConfig(t, `
[[bots]]
appid = "102000001"
secret = "from-file"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bots[0].AppID != "102999999" || cfg.Bots[0].Secret != "from-env" {
		t.Fatalf("单 Bot 的环境变量覆盖未生效: %+v", cfg.Bots[0])
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("单 Bot 覆盖不该告警: %v", cfg.Warnings)
	}
}

// 多 Bot 时环境变量只覆盖第一个并 warn：多值环境变量没有可用的表达形式，
// 宁可吵不要静默——用户会以为所有 Bot 都换成了环境里的凭据。
func TestEnvMultiBotWarnsAndOverridesFirst(t *testing.T) {
	t.Setenv("QQ_APP_ID", "102999999")
	t.Setenv("QQ_APP_SECRET", "from-env")
	path := writeConfig(t, `
[[bots]]
appid = "102000001"
secret = "s1"

[[bots]]
appid = "102000002"
secret = "s2"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bots[0].AppID != "102999999" || cfg.Bots[0].Secret != "from-env" {
		t.Errorf("第一个 Bot 应被环境变量覆盖: %+v", cfg.Bots[0])
	}
	if cfg.Bots[1].AppID != "102000002" || cfg.Bots[1].Secret != "s2" {
		t.Errorf("其余 Bot 不该被动: %+v", cfg.Bots[1])
	}
	warn := strings.Join(cfg.Warnings, "\n")
	for _, want := range []string{"只覆盖了", "多值环境变量没有可用的表达形式", "102999999"} {
		if !strings.Contains(warn, want) {
			t.Errorf("多 Bot 环境变量覆盖必须告警并写明原因，告警 %q 缺少 %q", warn, want)
		}
	}
}

// 一份只写了骨架、凭据全走环境变量的配置（容器里的既有用法）不能因为
// 改成数组就断掉：没有 [[bots]] 也没有 [app] 时，环境变量自己构成一个 Bot。
func TestEnvOnlyCredentialsBuildOneBot(t *testing.T) {
	t.Setenv("QQ_APP_ID", "102000777")
	t.Setenv("QQ_APP_SECRET", "env-secret")
	path := writeConfig(t, `
[store]
path = "./data/qqbot.db"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("纯环境变量凭据应能启动: %v", err)
	}
	if len(cfg.Bots) != 1 || cfg.Bots[0].AppID != "102000777" {
		t.Fatalf("应由环境变量构成一个 Bot，实际 %+v", cfg.Bots)
	}
}

// 校验错误必须指名是哪个 Bot（appid 或数组下标），否则多 Bot 现场只能逐个试。
func TestValidationNamesTheOffendingBot(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "认证类型非法",
			body: `
[[bots]]
appid = "102000001"
secret = "s"

[[bots]]
appid = "102000002"
secret = "s"
cert_type = "company"
`,
			want: []string{"bots[1]", "102000002", "cert_type"},
		},
		{
			name: "配额档含非法字符",
			body: `
[[bots]]
appid = "102000001"
secret = "s"
quota_profile = "wide open"
`,
			want: []string{"bots[0]", "102000001", "quota_profile"},
		},
		{
			name: "appid 重复",
			body: `
[[bots]]
appid = "102000001"
secret = "s"

[[bots]]
appid = "102000001"
secret = "s"
`,
			want: []string{"重复", "bots[1]", "102000001"},
		},
		{
			name: "appid 会被拼进数据目录，不能带路径",
			body: `
[[bots]]
appid = "../../etc"
secret = "s"
`,
			want: []string{"非法字符", "bots[0]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("应当报错")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误串 %q 缺少 %q", err, want)
				}
			}
		})
	}
}

// 文件里手抄的 appid/secret 可能带首尾空格。appid 会被拼进数据目录与锁文件名，
// secret 会进 Authorization 请求头，所以归一化必须写回配置，而不是只在比较时 Trim
// ——否则校验通过、运行时带着空格去连，报错点离病因很远。
func TestBotFieldsAreTrimmed(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	path := writeConfig(t, `
[[bots]]
appid = " 102000001 "
secret = " s "
data_dir = " /srv/qqbot/manual "
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Bots[0]
	if b.AppID != "102000001" || b.Secret != "s" {
		t.Errorf("凭据未归一化: appid=%q secret=%q", b.AppID, b.Secret)
	}
	if got := cfg.BotDataDir(b); got != "/srv/qqbot/manual" {
		t.Errorf("data_dir 未归一化: %q", got)
	}
}

// 一个 Bot 都没登记时，错误仍要指向 init 与字段名（#42 的验收语义）：
// 这是新用户看到的第一条错误，不能只说"配置不完整"。
func TestNoBotsPointsToInit(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")
	path := writeConfig(t, `
[store]
path = "./data/qqbot.db"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("没有 Bot 时应当报错")
	}
	for _, want := range []string{"qqbot init", "[app].id", "[[bots]].appid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误串 %q 缺少 %q", err, want)
		}
	}
}

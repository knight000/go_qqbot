package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 脱敏审计（#69）：配置层的每一条错误串都不许把 secret 原文带出来。
//
// 错误链是 secret 最容易漏的地方：校验失败时顺手 `%q` 一下"出问题的那个值"
// 是本能，而这份错误最后会进启动日志、进 journald、进用户贴到 issue 的截图。
// 允许出现的是 appid 与 cert_type——那两个是定位用的，不是凭据。
const auditSecret = "SECRET-AUDIT-9f3a7c"

// auditLetters 是全字母的凭据：TOML 解析器回显"出错的那个词"时只在字母上
// 断开，所以纯字母是最坏的情况（会被整段回显）。
const auditLetters = "SECRETAUDITMARKER"

// 每一项都制造一条真实的校验失败，且配置里带着 secret。
func TestValidationErrorsCarryNoSecret(t *testing.T) {
	base := func() *Config {
		return &Config{
			Store: Store{Path: "data/qqbot.db"},
			Bots:  []Bot{{AppID: "102000001", Secret: auditSecret, CertType: CertPersonal}},
			Log:   Log{Level: "info", Format: "text"},
			Bot:   BotOptions{Intents: 1, ShardTotal: 1},
			Debug: Debug{Level: DebugFull},
		}
	}

	cases := map[string]func(*Config){
		"cert_type 非法": func(c *Config) { c.Bots[0].CertType = "bogus" },
		"appid 含非法字符":  func(c *Config) { c.Bots[0].AppID = "1020/0001" },
		"appid 为空":     func(c *Config) { c.Bots[0].AppID = "" },
		"appid 重复": func(c *Config) {
			c.Bots = append(c.Bots, Bot{AppID: "102000001", Secret: auditSecret})
		},
		"quota_profile 非法": func(c *Config) { c.Bots[0].QuotaProfile = "not a slug" },
		"配额为负": func(c *Config) {
			c.Bots[0].QuotaProfile = "tight"
			c.Quota = map[string]QuotaProfile{"tight": {GroupQPM: -1}}
		},
		"log.level 非法": func(c *Config) { c.Log.Level = "verbose" },
		"debug.raw 与 plugin 冲突": func(c *Config) {
			c.Debug.Level, c.Debug.Raw = DebugPlugin, true
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s：本该校验失败，却通过了", name)
			}
			if strings.Contains(err.Error(), auditSecret) {
				t.Fatalf("%s：错误串里出现了 secret 原文：%v", name, err)
			}
		})
	}

	// 脱敏不能顺手把定位信息也删掉：每 Bot 的校验错误必须点名到 appid
	// （而 appid 本身不是凭据）。诊断信息与凭据在同一个字段附近，
	// 压错一个就是"不泄露但也查不出"。
	cfg := base()
	cfg.Bots[0].CertType = "bogus"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "appid=102000001") {
		t.Fatalf("每 Bot 的校验错误应点名 appid，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "cert_type") {
		t.Fatalf("错误串应指出是 cert_type 写错了，实际 %v", err)
	}
}

// secret 为空这条反向也要审：提示语里只能说"为空"，不能把旁边的值带出来。
func TestEmptySecretMessageCarriesNoSecret(t *testing.T) {
	cfg := &Config{
		Store: Store{Path: "data/qqbot.db"},
		Log:   Log{Level: "info", Format: "text"},
		Bots:  []Bot{{AppID: "102000001"}, {AppID: "102000002", Secret: auditSecret}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "secret 为空") {
		t.Fatalf("应提示 secret 为空，实际 %v", err)
	} else if strings.Contains(err.Error(), auditSecret) {
		t.Fatalf("提示语里出现了另一个 Bot 的 secret：%v", err)
	}
}

// Load 的解析与归一化错误同样不许带 secret：那份错误由启动期直接打印。
func TestLoadErrorsCarryNoSecret(t *testing.T) {
	cases := []struct {
		name string
		body string
		// marker 是这份配置里"绝不该出现在错误串里"的那段凭据。
		marker string
	}{
		{
			// 新旧两种形态同时出现：normalize 明确拒绝，并且要在同一句话里说清迁移动作。
			name: "[app] 与 [[bots]] 并存",
			body: `
[app]
id = "102000001"
secret = "` + auditSecret + `"

[[bots]]
appid = "102000002"
secret = "` + auditSecret + `"
`,
			marker: auditSecret,
		},
		{
			// 枚举值写错最容易被顺手 %q 出去。
			name: "cert_type 写错",
			body: `
[[bots]]
appid = "102000001"
secret = "` + auditSecret + `"
cert_type = "personal-cert"
`,
			marker: auditSecret,
		},
		{
			// 语法坏掉的那一行：TOML 解析器会回显"出错的那个词"，而 secret
			// 那一行少写一对引号正是这里最常见的现场。用一份**全是字母**的
			// 凭据是这条路径上最坏的情况——纯字母会被解析器整段回显。
			name: "secret 那一行少了引号",
			body: `
[[bots]]
appid = "102000001"
secret = ` + auditLetters + `
`,
			marker: auditLetters,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("写配置: %v", err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("%s：本该加载失败", tc.name)
			}
			if strings.Contains(err.Error(), tc.marker) {
				t.Fatalf("%s：错误串里出现了 secret 原文：%v", tc.name, err)
			}
			// 语法错误那条不能把定位信息一起抹掉：行号得在。
			if strings.Contains(tc.name, "少了引号") && !strings.Contains(err.Error(), "第 4 行") {
				t.Errorf("%s：错误串应指到行号，实际 %v", tc.name, err)
			}
		})
	}
}

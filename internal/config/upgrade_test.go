package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

// 默认值必须是"能直接用"的：没写 [upgrade] 的旧配置升级之后不能因为
// 空地址而静默失效，也不能因为默认关闭而让人以为命令坏了。
func TestUpgradeDefaults(t *testing.T) {
	c := &Config{}
	c.applyDefaults()

	if c.Upgrade.APIBase != "https://api.github.com" {
		t.Errorf("api_base 默认值 = %q", c.Upgrade.APIBase)
	}
	// #70 之前这里是 asset_base 的默认值。二进制通道现在由 source 决定，
	// 默认档是 official，asset_base 只在写了的时候才存在（兼容别名）。
	if got := c.Upgrade.AssetSource().Source; got != "official" {
		t.Errorf("默认档位 = %q", got)
	}
	if c.Upgrade.AssetBase != "" {
		t.Errorf("asset_base 不该有默认值（它是兼容别名）= %q", c.Upgrade.AssetBase)
	}
	if got := c.Upgrade.AssetSource().Template; got != "https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}" {
		t.Errorf("默认模板 = %q", got)
	}
	if !c.UpgradeEnabled() {
		t.Error("enabled 未配置时应为 true")
	}
	if c.Upgrade.Token != "" {
		t.Error("token 不该有默认值")
	}
}

func TestUpgradeDisabledExplicitly(t *testing.T) {
	c := &Config{Upgrade: Upgrade{Enabled: boolPtr(false)}}
	c.applyDefaults()
	if c.UpgradeEnabled() {
		t.Error("显式 false 应关掉自更新")
	}
}

// 四段配置都要能从 TOML 读进来：token 里常见的下划线与感叹号不能被吞。
func TestUpgradeSectionParses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	const body = `
[app]
id = "1"
secret = "s"

[store]
path = "./data/qqbot.db"

[upgrade]
enabled = false
api_base = "https://api.example.test"
asset_base = "https://mirror.example.test"
token = "github_pat_abc_123"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.UpgradeEnabled() {
		t.Error("enabled=false 没生效")
	}
	if c.Upgrade.APIBase != "https://api.example.test" ||
		c.Upgrade.AssetBase != "https://mirror.example.test" ||
		c.Upgrade.Token != "github_pat_abc_123" {
		t.Errorf("[upgrade] 解析结果不对: %+v", c.Upgrade)
	}
}

// #70 验收：三档各一条，断言**拼出的完整 URL**，而不是"不报错"。
func TestUpgradeSourceRendersCompleteURL(t *testing.T) {
	const (
		owner = "knight000"
		repo  = "go_qqbot"
		tag   = "v0.1.0"
		asset = "qqbot-0.1.0-linux-amd64"
	)
	cases := []struct {
		name     string
		section  string
		wantTmpl string
		wantURL  string
	}{
		{
			name:     "official（不写 source）",
			section:  "",
			wantTmpl: "https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}",
			wantURL:  "https://github.com/knight000/go_qqbot/releases/download/v0.1.0/qqbot-0.1.0-linux-amd64",
		},
		{
			name:     "mirror（预设 gh-proxy）",
			section:  "source = \"mirror\"",
			wantTmpl: "https://gh-proxy.com/https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}",
			wantURL:  "https://gh-proxy.com/https://github.com/knight000/go_qqbot/releases/download/v0.1.0/qqbot-0.1.0-linux-amd64",
		},
		{
			name:     "mirror（预设留空 = gh-proxy）",
			section:  "source = \"mirror\"\nmirror = \"\"",
			wantTmpl: "https://gh-proxy.com/https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}",
			wantURL:  "https://gh-proxy.com/https://github.com/knight000/go_qqbot/releases/download/v0.1.0/qqbot-0.1.0-linux-amd64",
		},
		{
			name:     "mirror（预设 ghproxy-net）",
			section:  "source = \"mirror\"\nmirror = \"ghproxy-net\"",
			wantTmpl: "https://ghproxy.net/https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}",
			wantURL:  "https://ghproxy.net/https://github.com/knight000/go_qqbot/releases/download/v0.1.0/qqbot-0.1.0-linux-amd64",
		},
		{
			name: "custom（完整模板）",
			section: `source = "custom"
asset_url = "https://cache.example/gh/{owner}/{repo}/{tag}/{asset}"`,
			wantTmpl: "https://cache.example/gh/{owner}/{repo}/{tag}/{asset}",
			wantURL:  "https://cache.example/gh/knight000/go_qqbot/v0.1.0/qqbot-0.1.0-linux-amd64",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadUpgradeConfig(t, tc.section)
			src := c.Upgrade.AssetSource()
			if src.Template != tc.wantTmpl {
				t.Errorf("模板 = %q want %q", src.Template, tc.wantTmpl)
			}
			if got := src.Render(owner, repo, tag, asset); got != tc.wantURL {
				t.Errorf("拼出的地址 = %q\nwant %q", got, tc.wantURL)
			}
		})
	}
}

// 只写了 mirror 却没写 source：必须按镜像走，不能静默回落官方。
//
// 这一条是"配错立刻报错"的同一动机：写了镜像却没生效，与本票要消灭的那类
// 意外完全同形（用户以为走了镜像、实际在直连）。
func TestUpgradeMirrorWithoutSourceStillUsesMirror(t *testing.T) {
	c := loadUpgradeConfig(t, `mirror = "ghproxy-net"`)
	src := c.Upgrade.AssetSource()
	if src.Source != "mirror" || src.Preset != "ghproxy-net" {
		t.Fatalf("只写 mirror 时应按镜像走，实际 source=%q preset=%q", src.Source, src.Preset)
	}
	want := "https://ghproxy.net/https://github.com/o/r/releases/download/v1.2.3/a"
	if got := src.Render("o", "r", "v1.2.3", "a"); got != want {
		t.Errorf("拼出的地址 = %q want %q", got, want)
	}

	// 未知名预设同样要报错，而不是"没写 source 就忽略"。
	if _, err := Load(writeUpgradeConfig(t, `mirror = "ghfast"`)); err == nil ||
		!strings.Contains(err.Error(), "不是已知预设") {
		t.Fatalf("未知名预设应让启动失败，实际: %v", err)
	}
}

// 老写法 asset_base 的行为必须逐字不变：拼出的 URL 与 #70 之前一模一样。
//
// 两种现场都钉住：显式写成 github.com（等价官方）与"把整个 github 地址塞进
// 前缀"（#70 之前唯一能走通镜像的写法）。
func TestUpgradeAssetBaseLegacyCompatibility(t *testing.T) {
	cases := []struct {
		name    string
		section string
		wantURL string
	}{
		{
			name:    "asset_base = github.com（等价官方）",
			section: `asset_base = "https://github.com"`,
			wantURL: "https://github.com/o/r/releases/download/v1.2.3/a",
		},
		{
			name:    "asset_base 塞进完整 github 地址（#70 之前的镜像写法）",
			section: `asset_base = "https://gh-proxy.com/https://github.com"`,
			wantURL: "https://gh-proxy.com/https://github.com/o/r/releases/download/v1.2.3/a",
		},
		{
			name:    "asset_base 带尾部斜杠",
			section: `asset_base = "https://mirror.example/"`,
			wantURL: "https://mirror.example/o/r/releases/download/v1.2.3/a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadUpgradeConfig(t, tc.section)
			if got := c.Upgrade.AssetSource().Render("o", "r", "v1.2.3", "a"); got != tc.wantURL {
				t.Errorf("拼出的地址 = %q want %q", got, tc.wantURL)
			}
		})
	}

	// 与官方通道同值时不必自称 custom：日志里出现一个不存在的"自定义通道"
	// 只会让人以为配错了。
	if src := loadUpgradeConfig(t, `asset_base = "https://github.com"`).Upgrade.AssetSource(); src.Source != "official" {
		t.Errorf("asset_base 等于官方地址时应归到 official，实际 %q", src.Source)
	}
}

// 五类配错逐条断言**启动失败**，且文案列出可用项。
//
// 这不是"风格问题"：用户以为走了镜像、实际在直连，只会表现为升级慢到撞上
// 15 分钟上限——那时已经浪费了一次完整的下载。
func TestUpgradeSourceMisconfigurationFailsStartup(t *testing.T) {
	cases := []struct {
		name    string
		section string
		want    []string
	}{
		{
			name:    "source 非法",
			section: `source = "gitee"`,
			want:    []string{`[upgrade].source="gitee" 非法`, "official / mirror / custom"},
		},
		{
			name:    "mirror 未知名",
			section: "source = \"mirror\"\nmirror = \"ghfast\"",
			want:    []string{`[upgrade].mirror="ghfast" 不是已知预设`, "gh-proxy / ghproxy-net"},
		},
		{
			name:    "custom 没有模板",
			section: `source = "custom"`,
			want:    []string{`source = "custom" 但没有模板`, "asset_url"},
		},
		{
			name:    "模板缺 {asset}",
			section: "source = \"custom\"\nasset_url = \"https://cache.example/gh/{owner}/{repo}/{tag}\"",
			want:    []string{"缺 {asset}"},
		},
		{
			name:    "模板含未知占位符",
			section: "source = \"custom\"\nasset_url = \"https://cache.example/gh/{owner}/{repo}/{tags}/{asset}\"",
			want:    []string{"未知占位符 {tags}"},
		},
		{
			name:    "模板不是 http(s) 绝对地址",
			section: "source = \"custom\"\nasset_url = \"cache.example/gh/{asset}\"",
			want:    []string{"不是 http(s) 绝对地址"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeUpgradeConfig(t, tc.section)
			_, err := Load(path)
			if err == nil {
				t.Fatal("配错必须让启动失败，实际通过了校验")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误文案缺少 %q：\n%s", want, err)
				}
			}
		})
	}
}

// 显式选了某档之后，另外几个键就是"写了但不生效"：不报错，但必须提醒。
func TestUpgradeSourceWarnsAboutIgnoredKeys(t *testing.T) {
	c := loadUpgradeConfig(t, `source = "official"
asset_base = "https://gh-proxy.com/https://github.com"`)
	warn := strings.Join(c.Warnings, "\n")
	if !strings.Contains(warn, "asset_base 不生效") {
		t.Errorf("显式 official 时 asset_base 应告警，实际 warnings: %v", c.Warnings)
	}

	c = loadUpgradeConfig(t, "source = \"mirror\"\nasset_url = \"https://cache.example/{asset}\"")
	warn = strings.Join(c.Warnings, "\n")
	if !strings.Contains(warn, "asset_url 不生效") || !strings.Contains(warn, "gh-proxy") {
		t.Errorf("显式 mirror 时 asset_url 应告警并点出预设，实际 warnings: %v", c.Warnings)
	}

	// 老写法不该被当成"写了但不生效"：它就是通道本身。
	if c := loadUpgradeConfig(t, `asset_base = "https://mirror.example"`); len(c.Warnings) != 0 {
		t.Errorf("老写法 asset_base 不该产生告警: %v", c.Warnings)
	}
}

// loadUpgradeConfig 写一份只带 [upgrade] 段的最小可启动配置并 Load。
func loadUpgradeConfig(t *testing.T, upgradeSection string) *Config {
	t.Helper()
	c, err := Load(writeUpgradeConfig(t, upgradeSection))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// writeUpgradeConfig 把 upgradeSection 原样拼进 [upgrade] 段。
func writeUpgradeConfig(t *testing.T, upgradeSection string) string {
	t.Helper()
	return writeConfig(t, `
[app]
id = "1"
secret = "s"

[store]
path = "./data/qqbot.db"

[upgrade]
`+upgradeSection+"\n")
}

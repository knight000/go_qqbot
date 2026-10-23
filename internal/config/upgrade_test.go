package config

import (
	"os"
	"path/filepath"
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
	if c.Upgrade.AssetBase != "https://github.com" {
		t.Errorf("asset_base 默认值 = %q", c.Upgrade.AssetBase)
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
path = "./.temp/data/qqbot.db"

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

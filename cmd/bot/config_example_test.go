package main

import (
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/host"
)

// 私有插件（开发工作区里 go generate 进来的那些）一旦声明 ConfigSchema，
// 如果也要求入库的 config.example.toml 写上它的段，就等于逼着开源仓库泄露
// 私有插件的名字：要么开发工作区恒红，要么把私有段提交进核心仓。
// 所以缺段时跳过并记日志，校验责任落回使用者自己的配置。
func TestPrivatePluginWithoutSectionIsSkipped(t *testing.T) {
	manifests := []host.Manifest{{
		Name:         "torn",
		ConfigSchema: map[string]string{"api_key": "string"},
	}}
	failures, skipped := checkExampleConfig(manifests, &config.Config{})
	if len(skipped) != 1 || skipped[0] != "torn" {
		t.Errorf("skipped = %v, want [torn]", skipped)
	}
	if len(failures) != 0 {
		t.Errorf("私有插件缺段不该让测试失败: %v", failures)
	}
}

// 白名单的意义是"照旧严格"，不是"全部放行"：示例插件缺段必须失败，
// 否则这份示例配置就不再是"复制一份就能启动"。
func TestExamplePluginWithoutSectionFails(t *testing.T) {
	manifests := []host.Manifest{{
		Name:         "echo",
		ConfigSchema: map[string]string{"reply_prefix": "string"},
	}}
	failures, skipped := checkExampleConfig(manifests, &config.Config{})
	if len(skipped) != 0 {
		t.Errorf("示例插件不该被跳过: %v", skipped)
	}
	if len(failures) != 1 {
		t.Errorf("示例插件缺段应当失败，实际 failures = %v", failures)
	}
}

// config.example.toml 是使用者复制一份就能启动的那份文件。插件声明了
// config_schema 之后，「声明即必填」意味着任何一个键缺失都会让新克隆的仓库
// 第一次启动就失败，所以这里对着磁盘上的真实文件跑一遍校验，而不是手写 map。
func TestExampleConfigSatisfiesEveryPluginSchema(t *testing.T) {
	var cfg config.Config
	if _, err := toml.DecodeFile(filepath.Join("..", "..", "config", "config.example.toml"), &cfg); err != nil {
		t.Fatalf("读取示例配置: %v", err)
	}

	plugins := host.Registered()
	if len(plugins) == 0 {
		t.Fatal("没有注册任何插件（先在 cmd/bot 下跑 go generate）")
	}
	manifests := make([]host.Manifest, 0, len(plugins))
	for _, p := range plugins {
		manifests = append(manifests, p.Manifest())
	}

	failures, skipped := checkExampleConfig(manifests, &cfg)
	for _, name := range skipped {
		t.Logf("跳过私有插件 %s：config.example.toml 没有 [plugins.%s] 段，"+
			"它的 schema 由使用者自己的配置满足（示例配置不替私有插件做承诺）", name, name)
	}
	for _, f := range failures {
		t.Errorf("示例配置不满足插件 schema: %s", f)
	}
}

// examplePlugins 是随核心仓一起发布的示例插件的**显式白名单**：只有它们必须
// 由入库的 config.example.toml 满足 schema。
//
// 用白名单而不是"名字前缀像不像"或"目录在不在 plugins/ 下"：私有插件与示例
// 插件在注册表里长得一模一样，唯一可靠的区分是"这个插件的配置段承诺出现在
// 开源仓库的示例里吗"，那是一个只有人知道的事实（评审 §3.3 P3-26）。
// 新增示例插件时要同步这里——漏了不会红，但测试日志里会打出"跳过私有插件 X"。
var examplePlugins = map[string]struct{}{
	"echo":       {},
	"adminping":  {},
	"bootnotice": {},
	"md":         {},
}

// checkExampleConfig 拿示例配置对照各插件 schema 跑一遍，返回失败项与被跳过
// 的私有插件名。
//
// 抽成纯函数是为了让"私有插件缺段"这条路径可构造：真实工作区里有没有私有
// 插件、它有没有声明 schema，取决于谁 clone 了仓库、装了什么插件。
func checkExampleConfig(manifests []host.Manifest, cfg *config.Config) (failures, skipped []string) {
	for _, m := range manifests {
		if len(m.ConfigSchema) == 0 {
			continue // 不声明 schema 的插件对示例配置没有要求
		}
		section := cfg.Plugin(m.Name)
		if _, isExample := examplePlugins[m.Name]; !isExample && len(section) == 0 {
			skipped = append(skipped, m.Name)
			continue
		}
		// 私有插件写了段就照常校验：使用者自己写进配置的键，写错了该报出来。
		if err := m.ValidateConfig(section); err != nil {
			failures = append(failures, err.Error())
		}
	}
	return failures, skipped
}

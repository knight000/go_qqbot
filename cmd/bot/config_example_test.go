package main

import (
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/host"
)

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
	for _, p := range plugins {
		m := p.Manifest()
		if err := m.ValidateConfig(cfg.Plugin(m.Name)); err != nil {
			t.Errorf("示例配置不满足插件 schema: %v", err)
		}
	}
}

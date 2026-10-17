// Package echo 是最小插件示例，同时充当插件契约的回归用例。
//
// 它演示了三件事：
//  1. 通过 init() 编译期注册（P1）；
//  2. Manifest 声明订阅事件与命令前缀；
//  3. 从 [plugins.echo] 读配置、通过 Host 回复。
//
// 复制这个包即可开始写新插件；业务插件不得 import internal/gateway。
package echo

import (
	"context"
	"log/slog"
	"strings"

	"github.com/knight000/qqbot/internal/host"
)

func init() { host.Register(&Plugin{}) }

type Plugin struct {
	prefix string
	log    *slog.Logger
}

func (p *Plugin) Manifest() host.Manifest {
	return host.Manifest{
		Name:         "echo",
		Version:      "0.1.0",
		APIVersion:   host.APIVersion,
		Capabilities: []string{"sender.reply", "log"},
		Intents:      []string{"message"},
		Commands: []host.Command{
			{Prefix: "!ping", Priority: 100, Description: "连通性检查"},
			{Prefix: "!echo", Priority: 100, Description: "回显文本"},
		},
		ConfigSchema: map[string]string{
			"reply_prefix": "string",
		},
	}
}

func (p *Plugin) Init(ctx context.Context, h host.Host) error {
	p.prefix = h.Config().Str("reply_prefix", "")
	p.log = h.Log()
	p.log.Info("echo 插件已初始化")
	return nil
}

func (p *Plugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	content := strings.TrimSpace(ev.Content)

	if strings.HasPrefix(content, "!ping") {
		return &host.Reply{Content: p.prefix + "pong"}, nil
	}

	if strings.HasPrefix(content, "!echo") {
		text := strings.TrimSpace(strings.TrimPrefix(content, "!echo"))
		if text == "" {
			return &host.Reply{Content: "用法：!echo <文本>"}, nil
		}
		return &host.Reply{Content: p.prefix + text}, nil
	}

	return nil, nil
}

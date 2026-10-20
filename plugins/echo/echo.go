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

	"github.com/knight000/go_qqbot/internal/host"
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

// 命令由路由层分派：命中的事件才投递过来，Event.Command 是命中的那条命令，
// Event.Args 是它之后已 TrimSpace 的剩余原文。插件不再用 HasPrefix 链判断。
func (p *Plugin) Handle(_ context.Context, ev *host.Event) (*host.Reply, error) {
	switch ev.Command {
	case "!ping":
		return &host.Reply{Content: p.prefix + "pong"}, nil

	case "!echo":
		if ev.Args == "" {
			return &host.Reply{Content: "用法：!echo <文本>"}, nil
		}
		return &host.Reply{Content: p.prefix + ev.Args}, nil
	}

	return nil, nil
}

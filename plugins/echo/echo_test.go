package echo

import (
	"context"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/host"
)

// newPlugin 用真实的 host.Service 当宿主：Host 接口很大，为两条命令写替身
// 反而会漏掉"Init 里读配置"这条路径。reply_prefix 是唯一用到的配置键。
func newPlugin(t *testing.T, replyPrefix string) *Plugin {
	t.Helper()
	h := host.NewService(host.Options{
		Name:   "echo",
		Config: host.NewParams("echo", map[string]any{"reply_prefix": replyPrefix}),
	})
	p := &Plugin{}
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p
}

// 分派只看 Event.Command：插件不再用 HasPrefix 链判断"是哪条命令"，
// 参数直接取 Event.Args（路由层已切好并 TrimSpace）。
func TestHandleDispatchesOnCommand(t *testing.T) {
	cases := []struct {
		name    string
		command string
		args    string
		want    string
		pass    bool // 不是本插件的命令 -> 放行
	}{
		{name: "ping", command: "!ping", want: "[bot] pong"},
		{name: "echo 带参数", command: "!echo", args: "你好 世界", want: "[bot] 你好 世界"},
		{name: "echo 空参数回用法", command: "!echo", args: "", want: "用法"},
		{name: "别的命令放行", command: "!other", pass: true},
		{name: "未命中命令放行", command: "", pass: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPlugin(t, "[bot] ")
			reply, err := p.Handle(context.Background(), &host.Event{
				ID: "e1", Kind: host.KindMessage, Scene: host.SceneGroup,
				// 故意不给 Content：分派只能靠 Command/Args，正文不许参与判断。
				Command: c.command, Args: c.args,
			})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if c.pass {
				if reply != nil {
					t.Fatalf("应放行，实际 %+v", reply)
				}
				return
			}
			if reply == nil || !strings.Contains(reply.Content, c.want) {
				t.Fatalf("回复 = %+v, want 含 %q", reply, c.want)
			}
		})
	}
}

// 两条命令都必须出现在 Manifest 里，否则路由层不会把它们投递过来。
func TestManifestDeclaresBothCommands(t *testing.T) {
	m := (&Plugin{}).Manifest()
	got := map[string]bool{}
	for _, c := range m.Commands {
		got[c.Prefix] = true
	}
	for _, want := range []string{"!ping", "!echo"} {
		if !got[want] {
			t.Fatalf("Manifest 应声明 %q，实际 %+v", want, m.Commands)
		}
	}
}

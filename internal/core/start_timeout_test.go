package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/pkg/host"
)

// hangInitPlugin 模拟"握手不回帧"的插件：Init 一直等到 ctx 到期才返回。
//
// 进程内替身做不到"真的不回帧"（那会连 ctx 都不理，只能靠杀进程），
// 但 core.Start 的期限恰好只在这一层生效——proxy.Init 把 ctx 一路带到
// rpc.CallInit，超时后返回 context.DeadlineExceeded。这个替身复现的正是
// 那条返回路径，而不用起子进程。
type hangInitPlugin struct {
	manifest host.Manifest
}

func (p *hangInitPlugin) Manifest() host.Manifest { return p.manifest }

func (p *hangInitPlugin) Init(ctx context.Context, _ host.Host) error {
	<-ctx.Done()
	return ctx.Err()
}

func (p *hangInitPlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}

// hangStartPlugin 的 Init 正常，Start 不回帧：同一条链路上的第二个缺口。
type hangStartPlugin struct {
	manifest host.Manifest
}

func (p *hangStartPlugin) Manifest() host.Manifest { return p.manifest }

func (p *hangStartPlugin) Init(context.Context, host.Host) error { return nil }

func (p *hangStartPlugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (p *hangStartPlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}

// 握手不返回时，启动必须在期限 + 余量内失败：卡在这里的进程活着、日志停在
// 上一行，比"插件崩溃"难查得多（#51）。
func TestStartFailsWhenPluginInitNeverReplies(t *testing.T) {
	t.Parallel()

	p := &hangInitPlugin{manifest: host.Manifest{Name: "hanginit", APIVersion: host.APIVersion}}
	c := newSchemaCore(t, &config.Config{}, p)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := time.Now()
	err := c.Start(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Init 不回帧应导致启动失败，而不是假装成功")
	}
	if elapsed > pluginInitTimeout+2*time.Second {
		t.Fatalf("启动应在 %v 的期限附近返回，实际用了 %v", pluginInitTimeout, elapsed)
	}
	for _, want := range []string{"hanginit", "timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应含 %q，实际: %v", want, err)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("超时错误应可 errors.Is(context.DeadlineExceeded)，实际: %v", err)
	}
}

// Start 不回帧同理：期限必须覆盖启动阶段，不只是 Init。
func TestStartFailsWhenPluginStartNeverReplies(t *testing.T) {
	t.Parallel()

	p := &hangStartPlugin{manifest: host.Manifest{Name: "hangstart", APIVersion: host.APIVersion}}
	c := newSchemaCore(t, &config.Config{}, p)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := time.Now()
	err := c.Start(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Start 不回帧应导致启动失败，而不是假装成功")
	}
	if elapsed > pluginStartTimeout+2*time.Second {
		t.Fatalf("启动应在 %v 的期限附近返回，实际用了 %v", pluginStartTimeout, elapsed)
	}
	for _, want := range []string{"hangstart", "timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应含 %q，实际: %v", want, err)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("超时错误应可 errors.Is(context.DeadlineExceeded)，实际: %v", err)
	}
}

package rpc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// 本文件钉住 `plugin.start` 交给插件的 ctx 的生命期。
//
// 踩过的坑：Conn.dispatch 对带期限的请求建的是 WithDeadline + defer cancel，
// 应答一写出就 cancel。如果 Start 拿的是这个请求 ctx，「Start 里 go p.run(ctx)」
// 起的后台协程会在 plugin.start 返回的同一瞬间退出——插件的日志里一条都看不到，
// 表现是启动通知、巡检、定时推送这类功能**静默不存在**。bootnotice 的启动通知
// 就是这么消失的（磁盘上的 last_start 一直停在几天前）。

// lifeCtxPlugin 记录 Start 拿到的 ctx，并在 Close 时报告它是否已被取消。
type lifeCtxPlugin struct {
	mu       sync.Mutex
	startCtx context.Context
	atClose  error
	closed   chan struct{}
}

func newLifeCtxPlugin() *lifeCtxPlugin {
	return &lifeCtxPlugin{closed: make(chan struct{})}
}

func (p *lifeCtxPlugin) Manifest() host.Manifest { return host.Manifest{Name: "lifectx"} }

func (p *lifeCtxPlugin) Init(context.Context, host.Host) error { return nil }

func (p *lifeCtxPlugin) Handle(context.Context, *host.Event) (*host.Reply, error) { return nil, nil }

func (p *lifeCtxPlugin) Start(ctx context.Context) error {
	p.mu.Lock()
	p.startCtx = ctx
	p.mu.Unlock()
	// 插件的真实写法：拿这个 ctx 起一个长活协程。
	go func() {
		<-ctx.Done()
		close(p.closed)
	}()
	return nil
}

func (p *lifeCtxPlugin) Close(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.startCtx != nil {
		p.atClose = p.startCtx.Err()
	}
	return nil
}

func (p *lifeCtxPlugin) snapshot() (context.Context, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startCtx, p.atClose
}

func TestStartCtxOutlivesTheCall(t *testing.T) {
	p := newLifeCtxPlugin()
	a, b := newPair(t)
	if err := ServePlugin(b, p); err != nil {
		t.Fatalf("ServePlugin: %v", err)
	}
	if err := ServeHost(a, func(string) (host.Host, error) {
		return host.NewService(host.Options{Name: "lifectx", Bot: host.BotInfo{AppID: "bot-a"}}), nil
	}); err != nil {
		t.Fatalf("ServeHost: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info := host.BotInfo{AppID: "bot-a", CertType: "personal"}
	if err := a.CallInit(ctx, info, "lifectx", nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	// CallStart 的 ctx 带期限——正是核心侧的调用形态（pluginStartTimeout）。
	if err := a.CallStart(ctx, info); err != nil {
		t.Fatalf("start: %v", err)
	}

	got, _ := p.snapshot()
	if got == nil {
		t.Fatal("Start 没有收到 ctx")
	}
	// 应答已经返回；此刻 ctx 必须还活着，否则「Start 里起协程」是句空话。
	select {
	case <-p.closed:
		t.Fatal("Start 的 ctx 在 plugin.start 返回时就取消了：后台协程活不过这一次调用")
	case <-time.After(300 * time.Millisecond):
	}
	if err := got.Err(); err != nil {
		t.Fatalf("Start 的 ctx 不应随调用结束而结束，实际：%v", err)
	}

	// 收摊时必须取消：协程不能活过进程级 close。
	if err := a.CallClose(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-p.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("plugin.close 之后 Start 的 ctx 仍未取消：后台协程会活过收摊")
	}
	if _, atClose := p.snapshot(); atClose == nil || atClose != context.Canceled {
		t.Fatalf("Close 里看到的 ctx.Err() = %v，期望 context.Canceled（取消要早于 Closer.Close）", atClose)
	}
}

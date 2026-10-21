package core

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

// gatedPlugin 的 Handle 卡在 gate 上，用来验证"关停要等在途事件处理完再关插件"。
// 它自己记账而不是靠外部探针：被 Close 之后才跑完的 Handle 要能被认出来。
type gatedPlugin struct {
	manifest host.Manifest
	gate     chan struct{}
	entered  chan struct{}

	mu        sync.Mutex
	closed    bool
	handled   int
	cancelled int // 处理途中被取消：真实插件在这里会返回错误、没有回复
	refused   int // Close 之后才完成的次数：出现就是关停顺序错了
}

func newGatedPlugin() *gatedPlugin {
	return &gatedPlugin{
		manifest: host.Manifest{
			Name:       "slow",
			Version:    "0.0.1",
			APIVersion: host.APIVersion,
			Intents:    []string{"message"},
			Commands:   []host.Command{{Prefix: "/slow", Priority: 100}},
		},
		gate:    make(chan struct{}),
		entered: make(chan struct{}, 16),
	}
}

func (p *gatedPlugin) Manifest() host.Manifest               { return p.manifest }
func (p *gatedPlugin) Init(context.Context, host.Host) error { return nil }

func (p *gatedPlugin) Handle(ctx context.Context, _ *host.Event) (*host.Reply, error) {
	p.entered <- struct{}{}
	select {
	case <-p.gate:
	case <-ctx.Done():
		// 真实插件在这里多半是一次 HTTP 或写库：ctx 取消就返回错误、没有回复。
		p.mu.Lock()
		p.cancelled++
		p.mu.Unlock()
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.refused++
		return nil, nil
	}
	p.handled++
	return &host.Reply{Content: "ok"}, nil
}

func (p *gatedPlugin) Close(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func (p *gatedPlugin) counts() (handled, refused int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handled, p.refused
}

// 有待处理消息时 Wait 必须一直阻塞，直到队列与在途的 Handle 都清空；
// 只等"在途"而不管排队的事件，关库时那条消息的回复与落库就断了。
func TestDispatcherWaitBlocksUntilQueueDrained(t *testing.T) {
	p := newGatedPlugin()
	d, f := newTestDispatcher(t, p)

	d.Dispatch(groupEvent("/slow", host.Principal{Role: host.RoleUser}))
	second := groupEvent("/slow", host.Principal{Role: host.RoleUser})
	second.ID = "e-2"
	second.MsgID = "m-2"
	d.Dispatch(second)
	<-p.entered // 第一条已进入 Handle，第二条还在队列里

	waited := make(chan bool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { waited <- d.Wait(ctx) }()

	select {
	case <-waited:
		t.Fatal("队列里还有事件时 Wait 不该返回")
	case <-time.After(80 * time.Millisecond):
	}

	close(p.gate)
	select {
	case ok := <-waited:
		if !ok {
			t.Fatal("Wait 报告超时，但事件本可以处理完")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("排空之后 Wait 仍未返回")
	}

	if handled, _ := p.counts(); handled != 2 {
		t.Fatalf("两条事件都应处理完，实际 %d", handled)
	}
	if got := f.callCount(); got != 2 {
		t.Fatalf("两条回复都应发出，实际 %d", got)
	}
}

// 网关 ctx 在关停第一步就被取消（main 的 stop()）。已经进入 Handle 的插件
// 不该跟着被打断：它正在写库、正在回消息，打断的代价是这条消息永远没有回复，
// 而平台不会再投第二次。
func TestDispatchKeepsRunningAfterGatewayContextCancelled(t *testing.T) {
	p := newGatedPlugin()
	c, f, _ := newShutdownCore(t, p)

	ctx, cancelGateway := context.WithCancel(context.Background())
	defer cancelGateway()

	c.Handle(ctx, adminEvent("/slow", "u1", "", host.SceneGroup, "g1"))
	<-p.entered

	// 这一步等价于 SIGTERM 之后 main 里的 stop()：网关 ctx 取消。
	cancelGateway()
	time.Sleep(20 * time.Millisecond) // 给"错误地跟着取消"留出窗口
	close(p.gate)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("关停应成功: %v", err)
	}
	if got := f.callCount(); got != 1 {
		t.Fatalf("网关 ctx 取消后这条消息的回复丢了（发出 %d 条）", got)
	}
}

// newShutdownCore 造一个带派发链路的最小 Core，群已放行。
func newShutdownCore(t *testing.T, p host.Plugin) (*Core, *fakeAPI, context.Context) {
	t.Helper()
	c, f := newAdminCore(t)
	router, err := NewRouter([]host.Plugin{p})
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	log := slog.New(slog.DiscardHandler)
	c.router = router
	c.dispatch = NewDispatcher(router, c.sender, log, 5*time.Second)
	c.plugins = []host.Plugin{p}
	c.sched = NewScheduler(c.store, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeAllow); err != nil {
		t.Fatal(err)
	}
	return c, f, ctx
}

// 关停顺序：先让 Dispatcher 排空，再关插件。反过来就是在途消息往已经关掉的
// 插件里写——用户看到的是"这条消息没回"，库里的落库也可能只写了一半。
func TestShutdownDrainsDispatchBeforeClosingPlugins(t *testing.T) {
	p := newGatedPlugin()
	c, f, ctx := newShutdownCore(t, p)

	c.Handle(ctx, adminEvent("/slow", "u1", "", host.SceneGroup, "g1"))
	<-p.entered

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Shutdown(shutdownCtx) }()

	select {
	case err := <-done:
		t.Fatalf("在途消息还没处理完，Shutdown 不该返回: %v", err)
	case <-time.After(80 * time.Millisecond):
	}

	close(p.gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("关停应成功: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("排空之后 Shutdown 仍未返回")
	}

	handled, refused := p.counts()
	if handled != 1 || refused != 0 {
		t.Fatalf("在途消息被丢掉了（handled=%d refused=%d）", handled, refused)
	}
	if got := f.callCount(); got != 1 {
		t.Fatalf("在途消息的回复应已发出，实际 %d", got)
	}
}

// 排空超时不能静默：返回值必须是 error，main 才有依据以非 0 退出。
func TestShutdownReportsTimeoutWhenDispatchStuck(t *testing.T) {
	p := newGatedPlugin()
	c, _, ctx := newShutdownCore(t, p)

	c.Handle(ctx, adminEvent("/slow", "u1", "", host.SceneGroup, "g1"))
	<-p.entered
	defer close(p.gate) // 释放卡住的 Handle，别把协程留到测试结束

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Shutdown(short); err == nil {
		t.Fatal("派发排空超时必须以 error 报告")
	}
}

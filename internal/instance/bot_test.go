package instance

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/internal/store"
)

// fakeConn 是 runGateway 的假连接：前 fail 次立刻返回错误，之后阻塞到
// ctx 取消。用它数"退避重连到底发生了几次"，不需要真的起 WebSocket。
type fakeConn struct {
	mu      sync.Mutex
	calls   int
	fail    int
	entered chan int
}

func (f *fakeConn) Run(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.entered != nil {
		select {
		case f.entered <- n:
		default:
		}
	}
	if n > f.fail {
		<-ctx.Done()
		return nil
	}
	return errors.New("机器人已封禁")
}

func (f *fakeConn) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newRuntimeForState 造一个只用于 runGateway 的运行期：真库（状态行预置成 st）
// 与真核心，连接是假的。
func newRuntimeForState(t *testing.T, st core.BotState) (*botRuntime, *core.Core) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/qqbot.db")
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	if err := core.SaveBotState(context.Background(), db, st); err != nil {
		t.Fatalf("预置状态: %v", err)
	}
	cc, err := core.New(core.Options{Store: db, Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("装配核心: %v", err)
	}
	return &botRuntime{
		cfg:      config.Bot{AppID: "102000001"},
		log:      slog.New(slog.DiscardHandler),
		coreImpl: cc,
		backoff:  []time.Duration{time.Millisecond},
		gwDone:   make(chan struct{}),
	}, cc
}

// failing 的 Bot 在网关退出之后必须继续重连，而不是收工——凭据修好后它要
// 能自己回来。非 failing 的退出保持原语义：一条 Error，连接放掉。
func TestFailingBotReconnectsWithBackoff(t *testing.T) {
	rt, _ := newRuntimeForState(t, core.BotStateFailing)
	conn := &fakeConn{fail: 2, entered: make(chan int, 4)}
	rt.conn = conn
	c := &Container{starting: true, fatal: make(chan error, 1)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rt.runGateway(ctx, c)

	deadline := time.After(15 * time.Second)
	for done := false; !done; {
		select {
		case n := <-conn.entered:
			done = n >= 3
		case <-deadline:
			t.Fatalf("failing 的 Bot 没有重连到第 3 次，目前只试了 %d 次", conn.callCount())
		}
	}

	cancel()
	select {
	case <-rt.gwDone:
	case <-time.After(15 * time.Second):
		t.Fatal("关停后 runGateway 应退出")
	}
	if got := conn.callCount(); got != 3 {
		t.Fatalf("第三次已连上（阻塞中），不该有第四次，实际 %d 次", got)
	}
}

// 非 failing 的永久错误不重连：这是 T2 定下的"一个坏租户不连累别人、也不
// 无限重试"的形态，failing 才接管退避。
func TestNonFailingBotStopsAfterGatewayExit(t *testing.T) {
	rt, _ := newRuntimeForState(t, core.BotStateActive)
	conn := &fakeConn{fail: 10}
	rt.conn = conn
	c := &Container{starting: true, fatal: make(chan error, 1)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rt.runGateway(ctx, c)

	select {
	case <-rt.gwDone:
	case <-time.After(3 * time.Second):
		t.Fatal("非 failing 的网关退出后 runGateway 应立即结束")
	}
	if got := conn.callCount(); got != 1 {
		t.Fatalf("非 failing 不该重连，实际 %d 次", got)
	}
}

// 配置里 enabled=false 的 Bot 不连接，但状态必须落成 suspended：库里留着
// 上次的 active/failing 会让排障的人误判成"它自己挂的"。
func TestDisabledBotIsMarkedSuspended(t *testing.T) {
	p := newFakePlatform(t)
	cfg := testConfig(t, p, "102000001", "102000002")
	off := false
	cfg.Bots[1].Enabled = &off

	c := startContainer(t, cfg, p)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Shutdown(ctx)
	}()

	if sts := c.Statuses(); len(sts) != 1 {
		t.Fatalf("停用的 Bot 不该进装配结果，实际 %d 个", len(sts))
	}
	p.waitReady(t, "102000001")

	for _, call := range gatewayCallsFor(p, "102000002") {
		t.Fatalf("停用的 Bot 不该取网关地址: %+v", call)
	}

	db := openStoreForTest(t, cfg.BotStorePath(cfg.Bots[1]))
	defer func() { _ = db.Close() }()
	st, err := core.LoadBotState(context.Background(), db)
	if err != nil {
		t.Fatalf("读停用 Bot 的状态: %v", err)
	}
	if st != core.BotStateSuspended {
		t.Fatalf("enabled=false 应落 suspended，实际 %q", st)
	}
}

// gatewayCallsFor 过滤出某个 appid 的取地址记录。
func gatewayCallsFor(p *fakePlatform, appID string) []gatewayCall {
	calls, _ := p.gatewaySnapshot()
	var out []gatewayCall
	for _, c := range calls {
		if c.appID == appID {
			out = append(out, c)
		}
	}
	return out
}

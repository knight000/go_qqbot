package rpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// fakePlugin 是驱动 pluginServer 的最小实现：只覆盖三条主路径
// （正常初始化、Init panic、Handle 卡住），不模拟真实插件的业务。
type fakePlugin struct {
	manifest  host.Manifest
	initPanic bool
	hang      chan struct{} // 非 nil 时 Handle 挂在这里，直到被关闭或 ctx 到期
}

func (p *fakePlugin) Manifest() host.Manifest { return p.manifest }

func (p *fakePlugin) Init(context.Context, host.Host) error {
	if p.initPanic {
		panic("测试用的初始化 panic")
	}
	return nil
}

func (p *fakePlugin) Handle(ctx context.Context, _ *host.Event) (*host.Reply, error) {
	if p.hang != nil {
		select {
		case <-p.hang:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &host.Reply{Content: "ok"}, nil
}

func serveFake(t *testing.T, p *fakePlugin) (*Conn, *Conn) {
	t.Helper()
	a, b := newPair(t)
	if err := ServePlugin(b, p); err != nil {
		t.Fatalf("ServePlugin: %v", err)
	}
	return a, b
}

// TestInitPanicClosesGate 是「装了但永不理人」的回归。
//
// Init panic 之后 gate 如果没关，每个 handle 都会挂在 wait 上直到 RPC 层
// 超时——症状是插件收得到事件、永远不回，而日志里只有一片 deadline。
// 关上门之后，后来者拿到的是**初始化失败**这条真实原因。
func TestInitPanicClosesGate(t *testing.T) {
	a, _ := serveFake(t, &fakePlugin{
		manifest:  host.Manifest{Name: "boom"},
		initPanic: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := a.CallInit(ctx, "boom", nil); !errors.Is(err, ErrHandlerPanic) {
		t.Fatalf("CallInit err = %v，期望 ErrHandlerPanic", err)
	}

	start := time.Now()
	_, err := a.CallHandle(ctx, &host.Event{ID: "e1"})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrHandlerPanic) {
		t.Fatalf("CallHandle err = %v，期望 ErrHandlerPanic（而不是超时）", err)
	}
	// 走 gate 的路径应当立刻返回；真等满 3 s 说明 gate 没关。
	if elapsed > time.Second {
		t.Fatalf("CallHandle 花了 %s，gate 没在 Init 失败后关闭", elapsed)
	}
}

// TestCallHandleDefaultTimeout：§3.6 的 3 s 口径由 RPC 层强制，不靠调用方
// 记得设——最慢的那个插件会把整个 per-peer 队列拖住。
func TestCallHandleDefaultTimeout(t *testing.T) {
	hang := make(chan struct{})
	a, _ := serveFake(t, &fakePlugin{
		manifest: host.Manifest{Name: "slow"},
		hang:     hang,
	})
	// 注册在 newPair 的清理之后 → LIFO 先跑：先放行 handler，再断连接。
	t.Cleanup(func() { close(hang) })

	start := time.Now()
	_, err := a.CallHandle(context.Background(), &host.Event{ID: "slow"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望 context.DeadlineExceeded", err)
	}
	if elapsed < HandleTimeout {
		t.Fatalf("只等了 %s，早于 %s 就放弃了", elapsed, HandleTimeout)
	}
	if elapsed > HandleTimeout+2*time.Second {
		t.Fatalf("等了 %s，远超 %s", elapsed, HandleTimeout)
	}
}

// TestCallHandleKeepsCallerDeadline：调用方给了时限就照用，别压回 3 s。
//
// 反过来说，如果这里被压低，测试与运维想设 30 s 调试就会被静默截断，
// 而症状只是「有时候莫名超时」。
func TestCallHandleKeepsCallerDeadline(t *testing.T) {
	hang := make(chan struct{})
	a, _ := serveFake(t, &fakePlugin{
		manifest: host.Manifest{Name: "slow"},
		hang:     hang,
	})
	t.Cleanup(func() { close(hang) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := a.CallHandle(ctx, &host.Event{ID: "slow"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望 context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("花了 %s，说明调用方的 100ms 被改成了默认值", elapsed)
	}
}

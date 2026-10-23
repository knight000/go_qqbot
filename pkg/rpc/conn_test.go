package rpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// newPair 造一对互通的连接，模拟 stdin/stdout 的两端。
//
// 用 io.Pipe 而不是 net.Pipe：前者在 Close 时让读写两侧立刻返回错误，
// 与「子进程退出、管道断开」的时序最接近。
func newPair(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	// 测试里把 handler panic 的堆栈丢掉：它是给线上排障用的，
	// 留在用例输出里只会淹没真正的失败。
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := NewConn(ar, aw, Options{Logger: quiet})
	b := NewConn(br, bw, Options{Logger: quiet})
	go func() { _ = a.Run() }()
	go func() { _ = b.Run() }()
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})
	return a, b
}

func TestConnCallServe(t *testing.T) {
	a, b := newPair(t)
	if err := b.Handle("echo", func(_ context.Context, req Request) (Response, error) {
		return Response{Result: struct{ Params map[string]any }{map[string]any{
			"got": string(req.Params),
		}}}, nil
	}); err != nil {
		t.Fatalf("注册: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := a.Call(ctx, "echo", struct{ N int }{7})
	if err != nil {
		t.Fatalf("调用: %v", err)
	}
	var out struct{ Params map[string]any }
	if err := res.Decode(&out); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if out.Params["got"] != `{"N":7}` {
		t.Fatalf("对端收到 %v", out.Params["got"])
	}
}

// TestConnMultiplex 是 ADR-0005 决策 4 的核心断言：一个慢请求不能把
// 同一连接上的其他调用排在后面。串行实现下这条用例会慢 200ms 以上。
func TestConnMultiplex(t *testing.T) {
	a, b := newPair(t)
	release := make(chan struct{})
	slowStarted := make(chan struct{})
	if err := b.Handle("slow", func(ctx context.Context, _ Request) (Response, error) {
		close(slowStarted)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Response{Result: struct{ Ok bool }{true}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Handle("fast", func(context.Context, Request) (Response, error) {
		return Response{Result: struct{ Ok bool }{true}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	slowDone := make(chan error, 1)
	go func() {
		_, err := a.Call(ctx, "slow", nil)
		slowDone <- err
	}()
	<-slowStarted

	start := time.Now()
	if _, err := a.Call(ctx, "fast", nil); err != nil {
		t.Fatalf("快请求: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("快请求等了 %v：慢请求把连接堵住了", elapsed)
	}
	close(release)
	if err := <-slowDone; err != nil {
		t.Fatalf("慢请求: %v", err)
	}
}

// TestConnBidirectional 覆盖「Init 里调 Host」这条路径：一个 handler 还在
// 跑的时候，它所在的那一端又发起了一个请求，且对端也要能应答。
func TestConnBidirectional(t *testing.T) {
	a, b := newPair(t)
	if err := a.Handle("host.config", func(context.Context, Request) (Response, error) {
		return Response{Result: struct{ Name string }{"testplugin"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Handle("plugin.init", func(ctx context.Context, _ Request) (Response, error) {
		// 模拟插件在 Init 里读配置：这条调用走的是 b → a，而 a 此刻正在等
		// plugin.init 的应答。
		res, err := b.Call(ctx, "host.config", nil)
		if err != nil {
			return Response{}, err
		}
		var out struct{ Name string }
		if err := res.Decode(&out); err != nil {
			return Response{}, err
		}
		return Response{Result: struct{ Got string }{out.Name}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := a.Call(ctx, "plugin.init", nil)
	if err != nil {
		t.Fatalf("Init 里回调 Host 失败: %v", err)
	}
	var out struct{ Got string }
	if err := res.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Got != "testplugin" {
		t.Fatalf("拿到 %q", out.Got)
	}
}

func TestConnMethodNotFound(t *testing.T) {
	a, b := newPair(t)
	_ = b
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := a.Call(ctx, "plugin.nope", nil)
	if !errors.Is(err, ErrMethodNotFound) {
		t.Fatalf("err = %v，期望 ErrMethodNotFound", err)
	}
}

// TestConnHandlerPanic：panic 必须被 recover 成错误，而且连接还能继续用。
func TestConnHandlerPanic(t *testing.T) {
	a, b := newPair(t)
	if err := b.Handle("boom", func(context.Context, Request) (Response, error) {
		panic("故意")
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Handle("ok", func(context.Context, Request) (Response, error) {
		return Response{Result: struct{}{}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := a.Call(ctx, "boom", nil); !errors.Is(err, ErrHandlerPanic) {
		t.Fatalf("err = %v，期望 ErrHandlerPanic", err)
	}
	if _, err := a.Call(ctx, "ok", nil); err != nil {
		t.Fatalf("panic 之后连接不可用了: %v", err)
	}
}

// TestConnDeadlinePropagated：调用方的 ctx 截止时间要捎给对端，
// 否则「核心 3s 后放弃」与「插件还在跑」之间没有任何联系（决策 7）。
func TestConnDeadlinePropagated(t *testing.T) {
	a, b := newPair(t)
	sawDeadline := make(chan bool, 1)
	if err := b.Handle("wait", func(ctx context.Context, _ Request) (Response, error) {
		_, ok := ctx.Deadline()
		sawDeadline <- ok
		<-ctx.Done()
		return Response{}, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := a.Call(ctx, "wait", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望 DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("超时用了 %v", elapsed)
	}
	select {
	case ok := <-sawDeadline:
		if !ok {
			t.Fatal("对端 handler 的 ctx 上没有截止时间")
		}
	case <-time.After(time.Second):
		t.Fatal("对端 handler 没被调用")
	}
}

// TestConnCloseFailsPending：连接一断，在途调用必须立刻失败而不是等超时。
func TestConnCloseFailsPending(t *testing.T) {
	a, b := newPair(t)
	if err := b.Handle("block", func(ctx context.Context, _ Request) (Response, error) {
		<-ctx.Done()
		return Response{}, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Call(ctx, "block", nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = a.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrConnClosed) {
			t.Fatalf("err = %v，期望 ErrConnClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("连接关了但调用还挂着")
	}
}

// TestConnHalfPacketErrors：对端写了两个字节就跑，读侧必须是错误
// （而不是一个「空帧」把后续字节全吃掉）。
func TestConnHalfPacketErrors(t *testing.T) {
	ar, bw := io.Pipe()
	c := NewConn(ar, io.Discard, Options{})
	errs := make(chan error, 1)
	go func() { errs <- c.Run() }()
	if _, err := bw.Write([]byte{0x00, 0x00}); err != nil {
		t.Fatalf("写半包: %v", err)
	}
	_ = bw.Close()
	select {
	case err := <-errs:
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("Run 返回 %v，期望 io.ErrUnexpectedEOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("半包之后 Run 没有返回")
	}
	// 收摊之后的新调用必须立刻失败，不能挂住。
	if _, err := c.Call(context.Background(), "x", nil); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("关停后调用 err = %v，期望 ErrConnClosed", err)
	}
}

// TestConnDuplicateHandle：同一个方法注册两次必须报错，静默覆盖会让
// 「谁抢了 host.reply」变成运行时谜题。
func TestConnDuplicateHandle(t *testing.T) {
	c := NewConn(bytes.NewReader(nil), io.Discard, Options{})
	h := func(context.Context, Request) (Response, error) { return Response{}, nil }
	if err := c.Handle("m", h); err != nil {
		t.Fatal(err)
	}
	if err := c.Handle("m", h); err == nil {
		t.Fatal("重复注册没有报错")
	}
	if err := c.Handle("", h); err == nil {
		t.Fatal("空方法名没有报错")
	}
	if err := c.Handle("m2", nil); err == nil {
		t.Fatal("nil handler 没有报错")
	}
	if !c.Handles("m") || c.Handles("m2") {
		t.Fatal("Handles 判断不对")
	}
}

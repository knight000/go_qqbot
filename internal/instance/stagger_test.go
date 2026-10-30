package instance

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 错峰的全部意义是"两次取地址之间有间隔"。这里断言的是间隔存在且不重叠，
// 不断言具体秒数：默认值会被配置改，测试钉死 30s 只会让改默认值的人去改测试。
func TestStaggerSerializesWithInterval(t *testing.T) {
	const interval = 40 * time.Millisecond
	s := NewStagger(interval)

	var mu sync.Mutex
	var spans [][2]time.Time
	inside := 0
	overlapped := false

	call := func(ctx context.Context) error {
		mu.Lock()
		inside++
		if inside > 1 {
			overlapped = true
		}
		start := time.Now()
		mu.Unlock()

		time.Sleep(5 * time.Millisecond)

		mu.Lock()
		inside--
		spans = append(spans, [2]time.Time{start, time.Now()})
		mu.Unlock()
		return nil
	}

	// 并发发起，模拟 N 个 Bot 同时启动：
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Do(context.Background(), call); err != nil {
				t.Errorf("Do: %v", err)
			}
		}()
	}
	wg.Wait()

	if overlapped {
		t.Fatal("两次取网关地址发生了重叠：串行没有生效")
	}
	if len(spans) != 3 {
		t.Fatalf("应执行 3 次，实际 %d 次", len(spans))
	}
	for i := 1; i < len(spans); i++ {
		gap := spans[i][0].Sub(spans[i-1][1])
		if gap < interval {
			t.Fatalf("第 %d 次与上一次的间隔 %s 小于 %s", i+1, gap, interval)
		}
	}
}

// 等待间隔时必须能被取消打断：关停时不该为了错峰白等 30s，更不该在
// 取消之后还发一次平台请求。
func TestStaggerCancelWhileWaiting(t *testing.T) {
	s := NewStagger(time.Hour)
	if err := s.Do(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("第一次 Do: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	called := false
	done := make(chan error, 1)
	go func() {
		done <- s.Do(ctx, func(context.Context) error {
			called = true
			return nil
		})
	}()
	// 让它确实进入等待，再取消。
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("等待被取消时应返回 context 错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 Do 没有返回：等待间隔无法被打断")
	}
	if called {
		t.Fatal("取消之后不该再执行取地址")
	}
}

// 间隔 0 = 只串行、不等待（显式关掉错峰的形态），不能退化成"禁止第二次"。
func TestStaggerZeroIntervalOnlySerializes(t *testing.T) {
	s := NewStagger(0)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := s.Do(context.Background(), func(context.Context) error { return nil }); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}
	if spent := time.Since(start); spent > time.Second {
		t.Fatalf("间隔 0 时不该等待，实际用了 %s", spent)
	}
}

// 解除之后必须直通：错峰只服务启动，运行期的重连不该为了排队白等一个间隔。
func TestStaggerDisarmStopsStaggering(t *testing.T) {
	s := NewStagger(time.Hour)
	// 先把 last 立起来：若闸门还在，后面任何一次调用都得等一小时。
	if err := s.Do(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("第一次 Do: %v", err)
	}
	s.Disarm()
	s.Disarm() // 可重复调用，不该 panic
	if s.Armed() {
		t.Fatal("Disarm 之后 Armed 应为 false")
	}

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Do(context.Background(), func(context.Context) error { return nil }); err != nil {
				t.Errorf("Do: %v", err)
			}
		}()
	}
	wg.Wait()
	if spent := time.Since(start); spent > time.Second {
		t.Fatalf("解除之后不该再等待，实际用了 %s", spent)
	}
}

// 卡在间隔等待里的调用也要被解除打断：启动循环末尾调 Disarm 时，可能正有
// 别的 Bot 排着队。
func TestStaggerDisarmInterruptsWaiting(t *testing.T) {
	s := NewStagger(time.Hour)
	if err := s.Do(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("第一次 Do: %v", err)
	}

	called := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Do(context.Background(), func(context.Context) error {
			called = true
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond) // 让它确实进入等待
	s.Disarm()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Disarm 之后等待中的 Do 没有返回")
	}
	if !called {
		t.Fatal("Disarm 只是取消等待，不该丢弃这次取地址")
	}
}

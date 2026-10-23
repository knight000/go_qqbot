package host

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSessionBackend 只记录契约层收敛后的入参：
// TTL 与 state 的边界条件都在 host 层判，后端看到的就是最终值。
type fakeSessionBackend struct {
	calls  int
	plugin string
	peer   string
	state  []byte
	ttl    time.Duration
	err    error
}

func (f *fakeSessionBackend) AwaitSession(_ context.Context, plugin string, ev *Event, state []byte, ttl time.Duration) error {
	f.calls++
	f.plugin, f.peer, f.state, f.ttl = plugin, ev.PeerID, state, ttl
	return f.err
}

func newSessionService(b SessionBackend) *Service {
	return NewService(Options{Name: "md", Session: b})
}

// TTL 是策略不是逻辑：算错的数不该让整条链路失败，所以只收敛、不报错。
func TestSessionTTLNormalized(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, DefaultSessionTTL},
		{-time.Minute, DefaultSessionTTL},
		{90 * time.Second, 90 * time.Second},
		{2 * time.Hour, MaxSessionTTL},
	}
	for _, c := range cases {
		b := &fakeSessionBackend{}
		svc := newSessionService(b)
		ev := &Event{PeerID: "g1", Scene: SceneGroup}

		if err := svc.Session().Await(context.Background(), ev, []byte("s"), c.in); err != nil {
			t.Fatalf("ttl=%s 应被接受: %v", c.in, err)
		}
		if b.ttl != c.want {
			t.Errorf("ttl=%s 应收敛成 %s，实际 %s", c.in, c.want, b.ttl)
		}
		if b.plugin != "md" {
			t.Errorf("后端应看到插件名 md，实际 %q", b.plugin)
		}
	}
}

// 超限直接报错、不许截断：截断的 JSON 是坏数据，插件下次读到只会得到更难查的错。
func TestSessionStateTooLarge(t *testing.T) {
	b := &fakeSessionBackend{}
	svc := newSessionService(b)

	big := make([]byte, MaxSessionStateBytes+1)
	err := svc.Session().Await(context.Background(), &Event{PeerID: "g1"}, big, 0)
	if err == nil {
		t.Fatal("超过上限应报错")
	}
	if b.calls != 0 {
		t.Fatalf("校验失败时后端不该被调用，实际 %d 次", b.calls)
	}

	// 恰好等于上限是允许的：边界值不该被差一错误挡在门外。
	if err := svc.Session().Await(context.Background(),
		&Event{PeerID: "g1"}, make([]byte, MaxSessionStateBytes), 0); err != nil {
		t.Fatalf("等于上限应被接受: %v", err)
	}
	if b.calls != 1 {
		t.Fatalf("后端应被调用 1 次，实际 %d", b.calls)
	}
}

// 没有 peer 就没有会话键，只能报错——静默成功会让插件以为已经等上了。
func TestSessionRequiresPeer(t *testing.T) {
	b := &fakeSessionBackend{}
	svc := newSessionService(b)

	for _, ev := range []*Event{nil, {Scene: SceneGroup}} {
		if err := svc.Session().Await(context.Background(), ev, nil, 0); err == nil {
			t.Fatalf("peer 为空应报错: %+v", ev)
		}
	}
	if b.calls != 0 {
		t.Fatalf("校验失败时后端不该被调用，实际 %d 次", b.calls)
	}
}

// 这是契约的脸面：core 把 store.ErrSessionExists 翻成这个哨兵，
// 插件只 import host，必须能用 errors.Is 认出来。
func TestSessionBusyIsClassifiable(t *testing.T) {
	b := &fakeSessionBackend{err: ErrSessionBusy}
	svc := newSessionService(b)

	err := svc.Session().Await(context.Background(), &Event{PeerID: "g1"}, nil, time.Minute)
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("插件侧应能 errors.Is 到 ErrSessionBusy，实际 %v", err)
	}
}

// 未装配后端时返回拒绝性替身而不是 nil：插件不必写判空分支。
func TestSessionWithoutBackend(t *testing.T) {
	svc := NewService(Options{Name: "md"})
	if svc.Session() == nil {
		t.Fatal("Session() 永不为 nil")
	}
	if err := svc.Session().Await(context.Background(), &Event{PeerID: "g1"}, nil, 0); err == nil {
		t.Fatal("未装配后端时调用应报错")
	}
}

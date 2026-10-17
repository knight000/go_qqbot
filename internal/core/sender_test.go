package core

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/knight000/qqbot/internal/api"
	"github.com/knight000/qqbot/internal/host"
)

type fakeAPI struct {
	calls  int
	bodies []api.MessageBody
}

func (f *fakeAPI) SendMessage(ctx context.Context, scene, peer string, body api.MessageBody) error {
	f.calls++
	f.bodies = append(f.bodies, body)
	return nil
}

func TestSenderPassiveLimitGroup(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{})
	ev := &host.Event{Scene: host.SceneGroup, PeerID: "g1", MsgID: "m1"}
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
			t.Fatalf("第 %d 次回复不应失败: %v", i, err)
		}
		if got := f.bodies[i-1].MsgSeq; got != i {
			t.Fatalf("msg_seq 应单调递增到 %d，实际 %d", i, got)
		}
	}
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, ErrPassiveExhausted) {
		t.Fatalf("第 6 次应耗尽额度，实际 err=%v", err)
	}
	if f.calls != 5 {
		t.Fatalf("耗尽后不应再发 HTTP，实际调用 %d 次", f.calls)
	}
}

func TestSenderPassiveLimitC2C(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{})
	ev := &host.Event{Scene: host.SceneC2C, PeerID: "u1", MsgID: "m2"}
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
			t.Fatalf("第 %d 次回复不应失败: %v", i+1, err)
		}
	}
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, ErrPassiveExhausted) {
		t.Fatalf("单聊第 5 次应耗尽额度，实际 err=%v", err)
	}
}

func TestSenderActiveDisabled(t *testing.T) {
	s := NewSender(&fakeAPI{}, slog.New(slog.DiscardHandler), ActivePolicy{})
	if err := s.SendActive(context.Background(), host.SceneGroup, "g1", host.Reply{Content: "x"}); !errors.Is(err, ErrActiveDisabled) {
		t.Fatalf("主动消息总开关关闭时应拒绝，实际 err=%v", err)
	}
}

// 插件白名单是硬边界：即使总开关打开，未声明的插件也不能发主动消息。
func TestSenderActiveRequiresPluginAllowlist(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"bootnotice": true},
		DailyPerPeer: 5,
	})

	if err := s.ForPlugin("evil").SendActive(context.Background(), host.SceneC2C, "u1", host.Reply{Content: "x"}); !errors.Is(err, ErrActiveDisabled) {
		t.Fatalf("非白名单插件应被拒绝，实际 err=%v", err)
	}
	if f.calls != 0 {
		t.Fatalf("被拒绝时不应发 HTTP，实际 %d 次", f.calls)
	}

	if err := s.ForPlugin("bootnotice").SendActive(context.Background(), host.SceneC2C, "u1", host.Reply{Content: "上线"}); err != nil {
		t.Fatalf("白名单插件应能发送: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("应发 1 次，实际 %d 次", f.calls)
	}
	if f.bodies[0].MsgID != "" {
		t.Errorf("主动消息不应带 msg_id，实际 %q", f.bodies[0].MsgID)
	}
}

// 本地日预算必须在发 HTTP 之前扣除，用尽后不再打扰平台。
func TestSenderActiveDailyBudget(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"bootnotice": true},
		DailyPerPeer: 2,
	})
	p := s.ForPlugin("bootnotice")
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err != nil {
			t.Fatalf("第 %d 次不应失败: %v", i+1, err)
		}
	}
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); !errors.Is(err, ErrActiveQuota) {
		t.Fatalf("第 3 次应超出日预算，实际 err=%v", err)
	}
	if f.calls != 2 {
		t.Fatalf("额度用尽后不应再发 HTTP，实际 %d 次", f.calls)
	}
	// 预算是按 peer 计的，换个收件人不受影响
	if err := p.SendActive(ctx, host.SceneC2C, "u2", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("其他收件人不应受影响: %v", err)
	}
}

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
	s := NewSender(f, slog.New(slog.DiscardHandler))
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
	s := NewSender(f, slog.New(slog.DiscardHandler))
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
	s := NewSender(&fakeAPI{}, slog.New(slog.DiscardHandler))
	if err := s.SendActive(context.Background(), host.SceneGroup, "g1", host.Reply{Content: "x"}); !errors.Is(err, ErrActiveDisabled) {
		t.Fatalf("主动消息应默认关闭，实际 err=%v", err)
	}
}

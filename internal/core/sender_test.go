package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/host"
)

// fakeAPI 记录每一次出站，并实现 groupInfoSource。
//
// 带锁不是洁癖：群生命周期与群名刷新都在后台协程里发消息、调接口，
// 断言与写入必然并发，裸切片会变成随机失败。
type fakeAPI struct {
	mu     sync.Mutex
	calls  int
	bodies []api.MessageBody

	// groupNames 是群信息接口的返回值；没写进 map 的群当作失败。
	names map[string]string
	// groupErr 让指定群返回错误（例如 11253）。
	errs map[string]error
	// infoCalls 记录接口调用次数，用于断言"已退出的群不该去刷"。
	infoCalls int

	// 上传相关：图片走 /files，与发消息分开计数。
	uploads    int
	uploadReqs []api.MediaUpload
	uploadErr  error
	// failSendAt 让第 N 次（1 起）发消息返回错误，用于验证"第二条失败"。
	failSendAt int
}

func (f *fakeAPI) SendMessage(ctx context.Context, scene, peer string, body api.MessageBody) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.bodies = append(f.bodies, body)
	if f.failSendAt > 0 && f.calls == f.failSendAt {
		return errors.New("模拟发送失败")
	}
	return nil
}

func (f *fakeAPI) UploadMedia(ctx context.Context, scene, peer string, up api.MediaUpload) (api.MediaUploadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads++
	f.uploadReqs = append(f.uploadReqs, up)
	if f.uploadErr != nil {
		return api.MediaUploadResult{}, f.uploadErr
	}
	return api.MediaUploadResult{
		FileUUID: fmt.Sprintf("uuid-%d", f.uploads),
		FileInfo: fmt.Sprintf("file-info-%d", f.uploads),
		TTL:      300,
	}, nil
}

func (f *fakeAPI) GroupInfo(ctx context.Context, groupOpenID string) (api.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCalls++
	if err := f.errs[groupOpenID]; err != nil {
		return api.GroupInfo{}, err
	}
	name, ok := f.names[groupOpenID]
	if !ok {
		return api.GroupInfo{}, errors.New("群不存在")
	}
	return api.GroupInfo{GroupOpenID: groupOpenID, GroupName: name}, nil
}

func (f *fakeAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeAPI) infoCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.infoCalls
}

func (f *fakeAPI) bodyAt(i int) api.MessageBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

func (f *fakeAPI) uploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads
}

func (f *fakeAPI) uploadAt(i int) api.MediaUpload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploadReqs[i]
}

func (f *fakeAPI) lastBody() (api.MessageBody, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return api.MessageBody{}, false
	}
	return f.bodies[len(f.bodies)-1], true
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
		if got := f.bodyAt(i - 1).MsgSeq; got != i {
			t.Fatalf("msg_seq 应单调递增到 %d，实际 %d", i, got)
		}
	}
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, ErrPassiveExhausted) {
		t.Fatalf("第 6 次应耗尽额度，实际 err=%v", err)
	}
	if n := f.callCount(); n != 5 {
		t.Fatalf("耗尽后不应再发 HTTP，实际调用 %d 次", n)
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
	if n := f.callCount(); n != 0 {
		t.Fatalf("被拒绝时不应发 HTTP，实际 %d 次", n)
	}

	if err := s.ForPlugin("bootnotice").SendActive(context.Background(), host.SceneC2C, "u1", host.Reply{Content: "上线"}); err != nil {
		t.Fatalf("白名单插件应能发送: %v", err)
	}
	if n := f.callCount(); n != 1 {
		t.Fatalf("应发 1 次，实际 %d 次", n)
	}
	if got := f.bodyAt(0).MsgID; got != "" {
		t.Errorf("主动消息不应带 msg_id，实际 %q", got)
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
	if n := f.callCount(); n != 2 {
		t.Fatalf("额度用尽后不应再发 HTTP，实际 %d 次", n)
	}
	// 预算是按 peer 计的，换个收件人不受影响
	if err := p.SendActive(ctx, host.SceneC2C, "u2", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("其他收件人不应受影响: %v", err)
	}
}

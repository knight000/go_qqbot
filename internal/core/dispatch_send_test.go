package core

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/host"
)

// blockingAPI 让 SendMessage 一直阻塞到 ctx 取消，模拟"在途大图上传"。
type blockingAPI struct {
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
	once2    sync.Once
}

func newBlockingAPI() *blockingAPI {
	return &blockingAPI{started: make(chan struct{}), canceled: make(chan struct{})}
}

func (b *blockingAPI) SendMessage(ctx context.Context, _ string, _ string, _ api.MessageBody) error {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	b.once2.Do(func() { close(b.canceled) })
	return ctx.Err()
}

func (b *blockingAPI) UploadMedia(context.Context, string, string, api.MediaUpload) (api.MediaUploadResult, error) {
	return api.MediaUploadResult{}, errors.New("本用例不该走上传")
}

// TestWaitCancelsInflightReply 钉住 #24 与 #25 的口径冲突：Reply 曾被
// context.WithoutCancel 派生（保留 5s/30s 预算，但谁也不能取消它），
// 于是一条 30s 上传会让 Wait 等满、让关库排在发送之后（写库撞
// "database is closed"）、让关停以非 0 退出。
//
// 现在发送挂在 Dispatcher 自己的 sendCtx 上：关停期限一到就掐断，
// handler 随即收尾，排空仍算成功。
func TestWaitCancelsInflightReply(t *testing.T) {
	p := newStub("slow", "/slow", host.PermUser, "")
	router, err := NewRouter([]host.Plugin{p})
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	f := newBlockingAPI()
	log := slog.New(slog.DiscardHandler)
	d := NewDispatcher(router, NewSender(f, log, ActivePolicy{}, nil, time.Local), log, time.Second)

	d.Dispatch(groupEvent("/slow", host.Principal{Role: host.RoleUser, GroupRole: host.GroupRoleMember}))
	select {
	case <-f.started:
	case <-time.After(3 * time.Second):
		t.Fatal("回复没有开始发送")
	}

	// 关停期限 200ms，远小于一条回复的 5s 预算（含上传是 30s）。
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	ok := d.Wait(ctx)
	elapsed := time.Since(start)

	if !ok {
		t.Fatalf("发送被掐断后 handler 能收尾，应算排空成功（实际耗时 %s）", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("关停不该等满发送预算，实际等了 %s", elapsed)
	}
	select {
	case <-f.canceled:
	default:
		t.Fatal("在途发送必须收到取消")
	}
}

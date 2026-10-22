package core

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/host"
)

// deadlineAPI 记下每次出站拿到的 ctx deadline。上传与发消息分开记，
// 才能分别断言"含上传 30s""纯文本 5s"。上传可以注入错误，用来模拟慢网超时。
type deadlineAPI struct {
	mu sync.Mutex

	sendDeadline   time.Time
	uploadDeadline time.Time
	uploadErr      error
}

func (f *deadlineAPI) SendMessage(ctx context.Context, scene, peer string, body api.MessageBody) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendDeadline, _ = ctx.Deadline()
	return nil
}

func (f *deadlineAPI) UploadMedia(ctx context.Context, scene, peer string, up api.MediaUpload) (api.MediaUploadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadDeadline, _ = ctx.Deadline()
	if f.uploadErr != nil {
		return api.MediaUploadResult{}, f.uploadErr
	}
	return api.MediaUploadResult{FileUUID: "uuid-1", FileInfo: "file-info-1", TTL: 300}, nil
}

func (f *deadlineAPI) lastUploadDeadline() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploadDeadline
}

func (f *deadlineAPI) lastSendDeadline() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendDeadline
}

// 含上传的回复必须拿到独立预算：上传的 30s 只放宽了 http.Client.Timeout，
// 放不宽 ctx。5 MB 图 base64 后约 6.7 MB，10 Mbps 上行要 5s 以上，
// 5s 的 ctx 让图永远发不出去。
//
// 预算由 Sender 按回复内容决定：插件拿不到 Reply 的错误，判断只能在核心内。
func TestReplyDeadlineCoversUpload(t *testing.T) {
	f := &deadlineAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.UTC)

	start := time.Now()
	if err := s.Reply(context.Background(), msgEvent(), host.Reply{
		Image: host.ImageFromBytes(tinyPNG()),
	}); err != nil {
		t.Fatalf("发图不应失败: %v", err)
	}

	up := f.lastUploadDeadline()
	if up.IsZero() {
		t.Fatal("上传没有拿到任何 deadline：ctx 里必须有独立预算")
	}
	if d := up.Sub(start); d < 30*time.Second {
		t.Fatalf("上传应至少拿到 30s 预算，实际 %v", d)
	}
	sent := f.lastSendDeadline()
	if d := sent.Sub(start); d < 30*time.Second {
		t.Fatalf("富媒体消息本身也应拿到 ≥30s（上传完成后还要发），实际 %v", d)
	}
}

// 纯文本沿用 5s：收紧到 30s 会让一个卡住的平台连接把派发协程占住半分钟。
func TestReplyTextDeadlineStaysFiveSeconds(t *testing.T) {
	f := &deadlineAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.UTC)

	if err := s.Reply(context.Background(), msgEvent(), host.Reply{Content: "hi"}); err != nil {
		t.Fatalf("文本回复不应失败: %v", err)
	}
	dl := f.lastSendDeadline()
	if dl.IsZero() {
		t.Fatal("文本回复应带回 5s deadline")
	}
	// 从 Reply 返回之后量剩余预算：不会因为临界区耗时把上界顶过 5s。
	if d := dl.Sub(time.Now()); d > 5*time.Second || d < 4*time.Second {
		t.Fatalf("纯文本预算应约为 5s，实际剩余 %v", d)
	}
}

// 上传超时不能表现成"文字已发出、图没有、日志全是成功"：
// 至少一条 Warn 要指得回是谁的哪条消息，且错误不能被吞成成功。
func TestReplyUploadTimeoutWarnsWithPeer(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	f := &deadlineAPI{uploadErr: context.DeadlineExceeded}
	s := NewSender(f, log, ActivePolicy{}, nil, time.UTC)

	err := s.Reply(context.Background(), msgEvent(), host.Reply{
		Content: "查询结果：",
		Image:   host.ImageFromBytes(tinyPNG()),
	})
	if err == nil {
		t.Fatal("上传超时必须返回错误，不能当成发送成功")
	}
	if !strings.Contains(err.Error(), "前 1 条已发出") {
		t.Fatalf("错误应说明文本已经发出去了，实际: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("上传失败至少要有一条 Warn 日志，实际日志: %q", out)
	}
	if !strings.Contains(out, "peer=g1") {
		t.Fatalf("Warn 日志必须带 peer（规则 7：错误要指得回它发生在谁身上），实际: %q", out)
	}
	if !strings.Contains(out, "deadline") {
		t.Fatalf("Warn 日志要带上失败原因，实际: %q", out)
	}
}

package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/httpx"
	"github.com/knight000/go_qqbot/internal/store"
)

// newUsageStore 开一个临时库，供主动消息日预算的测试使用（预算已落库）。
func newUsageStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	return db
}

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
	// bizSendAt 让第 N 次（1 起）发消息返回平台业务错误（HTTP 200 + err_code），
	// 与 failSendAt 的区别是"平台明确拒绝"而不是"网络层面不明"。
	bizSendAt int
	// rateLimitedAt 让第 N 次（1 起）发消息返回平台 429；retryAfter 模拟
	// Retry-After 头（0 表示平台没给，核心应按缺省窗口冷却）。
	rateLimitedAt int
	retryAfter    time.Duration
}

func (f *fakeAPI) SendMessage(ctx context.Context, scene, peer string, body api.MessageBody) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.bodies = append(f.bodies, body)
	if f.rateLimitedAt > 0 && f.calls == f.rateLimitedAt {
		return &httpx.HTTPError{Status: http.StatusTooManyRequests, RetryAfter: f.retryAfter}
	}
	if f.bizSendAt > 0 && f.calls == f.bizSendAt {
		return &api.BusinessError{Code: 40034005, Message: "msg_id 已过期"}
	}
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
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
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
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
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
	s := NewSender(&fakeAPI{}, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
	if err := s.SendActive(context.Background(), host.SceneGroup, "g1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrActiveDisabled) {
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
	}, newUsageStore(t), time.Local)

	if err := s.ForPlugin("evil").SendActive(context.Background(), host.SceneC2C, "u1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrActiveDisabled) {
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

// 窗口过期后序号不能回到 1：平台靠 (msg_id, msg_seq) 判重，
// 窗口只该重置"还剩几次"，不该重置"第几条"。
func TestSenderPassiveSeqKeepsRisingAfterWindowExpiry(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	ev := &host.Event{Scene: host.SceneGroup, PeerID: "g1", MsgID: "m1"}
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
			t.Fatalf("第 %d 次回复不应失败: %v", i, err)
		}
		if got := f.bodyAt(i - 1).MsgSeq; got != i {
			t.Fatalf("第 %d 条的 seq 应为 %d，实际 %d", i, i, got)
		}
	}
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, ErrPassiveExhausted) {
		t.Fatalf("窗口内第 6 次应耗尽，实际 err=%v", err)
	}

	// 群窗口是 5 分钟：跨过去只该拿回 5 格额度，序号连着数。
	now = now.Add(6 * time.Minute)
	for i := 6; i <= 10; i++ {
		if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
			t.Fatalf("过期后第 %d 条不应失败: %v", i, err)
		}
		if got := f.bodyAt(i - 1).MsgSeq; got != i {
			t.Fatalf("窗口过期后 seq 应继续递增到 %d，实际 %d", i, got)
		}
	}
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, ErrPassiveExhausted) {
		t.Fatalf("新窗口第 6 次仍应耗尽，实际 err=%v", err)
	}
}

// seqCap 是**软上界**：全部窗口都还在被动有效期内时，宁可暂时超出一条，
// 也不淘汰任何一条。被淘汰的那条 used 会清零（同一 msg_id 可以超发）、
// seq 会回到 1（(msg_id, msg_seq) 重复），比多占 ~64 字节严重得多。
func TestSenderPassiveSeqCapYieldsToLiveWindows(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
	ctx := context.Background()

	send := func(msgID string) int {
		t.Helper()
		ev := &host.Event{Scene: host.SceneGroup, PeerID: "g1", MsgID: msgID}
		if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
			t.Fatalf("%s 回复失败: %v", msgID, err)
		}
		body, _ := f.lastBody()
		return body.MsgSeq
	}

	for i := 0; i < seqCap; i++ {
		send(fmt.Sprintf("m%d", i))
	}
	// 这些窗口都刚建、5 分钟内不会过期，因此没有可淘汰的条目。
	send("fresh")
	if n := len(s.windows); n != seqCap+1 {
		t.Fatalf("没有过期条目时不该淘汰，实际条目 %d（期望 %d）", n, seqCap+1)
	}
	// m0 是最久未用的那条，但它仍在窗口内：seq 必须继续往上数。
	if got := send("m0"); got != 2 {
		t.Fatalf("仍在窗口内的 msg_id 被淘汰了：seq 从 1 重来（实际 %d）", got)
	}
}

// 达到 seqCap 后淘汰的只能是**窗口已过期**的条目。踢掉一条仍在用的窗口会
// 同时重置 passive 次数与 seq——那正是 #14 要消除的 (msg_id, msg_seq) 重复。
func TestSenderPassiveEvictionSkipsLiveWindows(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
	ctx := context.Background()
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	now := start
	s.now = func() time.Time { return now }

	send := func(scene host.Scene, peer, msgID string) int {
		t.Helper()
		ev := &host.Event{Scene: scene, PeerID: peer, MsgID: msgID}
		if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
			t.Fatalf("%s 回复失败: %v", msgID, err)
		}
		body, _ := f.lastBody()
		return body.MsgSeq
	}

	// live 是单聊（60 分钟窗口），故意让它成为"最久未用"的那一条：
	// 旧实现按 lastOrder 淘汰，会先踢它，而不是踢真正该清的过期群窗口。
	send(host.SceneC2C, "u1", "live")
	// 再塞满 cap-1 条群会话（5 分钟窗口）。
	for i := 0; i < seqCap-1; i++ {
		send(host.SceneGroup, "g1", fmt.Sprintf("old%d", i))
	}
	if n := len(s.windows); n != seqCap {
		t.Fatalf("前置条件不成立：条目应为 %d，实际 %d", seqCap, n)
	}

	// 群窗口早已过期，单聊窗口还剩 50 分钟。
	now = start.Add(10 * time.Minute)
	send(host.SceneGroup, "g1", "fresh") // 触发淘汰；候选只有已过期的 old*
	if n := len(s.windows); n != seqCap {
		t.Fatalf("淘汰应恰好删掉一条已过期条目，实际条目 %d", n)
	}
	// live 的 used/seq 一旦被清零，这里就会拿到 1。
	if got := send(host.SceneC2C, "u1", "live"); got != 2 {
		t.Fatalf("活跃窗口被淘汰：live 的 seq 从 1 重来（实际 %d）", got)
	}
}

// 序号条目最终要被回收，而不是只靠 seqCap 兜底：一条消息的 msg_id 在
// 窗口过期 24 小时后不可能再被重试，那时 seq 从头数就是安全的。
func TestSenderPassiveSeqRetentionExpires(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	ev := &host.Event{Scene: host.SceneGroup, PeerID: "g1", MsgID: "m1"}
	ctx := context.Background()

	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
		t.Fatalf("首次回复失败: %v", err)
	}

	now = now.Add(25 * time.Hour) // 窗口早已过期，序号保留期也过了
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
		t.Fatalf("保留期后回复失败: %v", err)
	}
	if got := f.bodyAt(1).MsgSeq; got != 1 {
		t.Fatalf("保留期过后应重新从 1 计数，实际 seq=%d", got)
	}
}

// 平台明确拒绝时，那一格被动配额要还回去：用户什么都没收到，账上却少一次，
// 连着被风控挡掉几条之后，正常回复也发不出去了。
func TestSenderPassiveSlotReleasedOnBusinessError(t *testing.T) {
	f := &fakeAPI{bizSendAt: 1}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
	ev := &host.Event{Scene: host.SceneGroup, PeerID: "g1", MsgID: "m1"}
	ctx := context.Background()

	err := s.Reply(ctx, ev, host.Reply{Content: "hi"})
	var be *api.BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("平台拒绝应原样透出业务错误，实际 err=%v", err)
	}
	// 还回一格 = 群窗口的 5 次仍然完整可用。
	for i := 1; i <= 5; i++ {
		if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
			t.Fatalf("归还后第 %d 次应能发出: %v", i, err)
		}
	}
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, ErrPassiveExhausted) {
		t.Fatalf("5 格用满后应耗尽，实际 err=%v", err)
	}
	// seq 不回退：被拒的那次已经用过 seq=1，(msg_id, msg_seq) 不能重来。
	if got := f.bodyAt(1).MsgSeq; got != 2 {
		t.Errorf("业务错误后 seq 不该回退，期望 2，实际 %d", got)
	}
}

// 网络失败时那一格不能还：请求可能已经到达平台，还回去等于允许实际发送
// 超过窗口上限（群 5 次/5 分钟、单聊 4 次/小时都是平台的硬约束）。
func TestSenderPassiveSlotKeptOnNetworkError(t *testing.T) {
	f := &fakeAPI{failSendAt: 1}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
	ev := &host.Event{Scene: host.SceneGroup, PeerID: "g1", MsgID: "m1"}
	ctx := context.Background()

	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err == nil {
		t.Fatal("模拟网络失败应返回错误")
	}
	for i := 1; i <= 4; i++ {
		if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
			t.Fatalf("第 %d 次应能发出: %v", i, err)
		}
	}
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, ErrPassiveExhausted) {
		t.Fatalf("网络失败已占一格，5 次后应耗尽，实际 err=%v", err)
	}
}

// 主动消息的日预算先扣再发；平台明确拒绝时这一格同样要还——账在库里，
// 不还就是永久烧掉当天的额度。
func TestSenderActiveBudgetRefundedOnBusinessError(t *testing.T) {
	f := &fakeAPI{bizSendAt: 1}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"bootnotice": true},
		DailyPerPeer: 1,
	}, newUsageStore(t), time.Local)
	p := s.ForPlugin("bootnotice")
	ctx := context.Background()

	err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"})
	var be *api.BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("平台拒绝应返回业务错误，实际 err=%v", err)
	}
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("归还后当天那 1 格应可用: %v", err)
	}
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("归还后仍只能发 1 条，第 3 次应超预算，实际 err=%v", err)
	}
}

// 上传被平台拒绝时预算也要还：主动消息的顺序是"扣预算 → 上传 → 发送"，
// 上传就失败的那条根本到不了平台的消息接口。
func TestSenderActiveBudgetRefundedOnUploadBusinessError(t *testing.T) {
	f := &fakeAPI{uploadErr: &api.BusinessError{Code: 1100101, Message: "内容被风控"}}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 1,
	}, newUsageStore(t), time.Local)
	p := s.ForPlugin("digest")
	ctx := context.Background()

	err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Image: host.ImageFromBytes(tinyPNG())})
	var be *api.BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("上传被拒绝应透出业务错误，实际 err=%v", err)
	}
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("归还后当天那 1 格应可用: %v", err)
	}
}

// 网络失败不归还主动预算：发没发出去是未知的。这是既定取舍——宁可少发
// 一条，也不能把平台每天 1000 条的配额当成无限量烧掉。
func TestSenderActiveBudgetKeptOnNetworkError(t *testing.T) {
	f := &fakeAPI{failSendAt: 1}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"bootnotice": true},
		DailyPerPeer: 1,
	}, newUsageStore(t), time.Local)
	p := s.ForPlugin("bootnotice")
	ctx := context.Background()

	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err == nil {
		t.Fatal("模拟网络失败应返回错误")
	}
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("网络失败应保留那一格，第 2 次应超预算，实际 err=%v", err)
	}
}

// 平台明确拒绝时轨迹必须是"发送失败"：轨迹里写着"已发送"是最误导人的一件事
// ——所有日志都指向成功，用户却什么都没收到。
func TestTraceSaysFailedOnBusinessError(t *testing.T) {
	f := &fakeAPI{bizSendAt: 1}
	var buf bytes.Buffer
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
	s.trace = NewTrace(&buf, LevelFull)
	ev := &host.Event{Scene: host.SceneGroup, PeerID: "g1", MsgID: "m1"}

	_ = s.Reply(context.Background(), ev, host.Reply{Content: "hi"})

	out := buf.String()
	if !strings.Contains(out, "发送失败") {
		t.Fatalf("平台拒绝时轨迹应写发送失败，实际 %q", out)
	}
	if strings.Contains(out, "已发送") {
		t.Fatalf("平台拒绝时轨迹不得写已发送，实际 %q", out)
	}
	// 错误码要出现在轨迹里，否则排障只能靠猜平台为什么拒。
	if !strings.Contains(out, "40034005") {
		t.Fatalf("轨迹应带平台错误码，实际 %q", out)
	}
}

// 本地日预算必须在发 HTTP 之前扣除，用尽后不再打扰平台。
func TestSenderActiveDailyBudget(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"bootnotice": true},
		DailyPerPeer: 2,
	}, newUsageStore(t), time.Local)
	p := s.ForPlugin("bootnotice")
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err != nil {
			t.Fatalf("第 %d 次不应失败: %v", i+1, err)
		}
	}
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrActiveQuota) {
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

// 日切按本地时区换桶：同一 peer 昨天用满，今天应该重新有一份完整预算。
func TestSenderActiveBudgetResetsNextDay(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"bootnotice": true},
		DailyPerPeer: 1,
	}, newUsageStore(t), time.Local)
	// 23:59 与 00:01 是同一个"日预算"问题里唯一值得卡住的边界。
	now := time.Date(2026, 10, 20, 23, 59, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	p := s.ForPlugin("bootnotice")
	ctx := context.Background()

	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("当日第 1 次不应失败: %v", err)
	}
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("当日第 2 次应超出日预算，实际 err=%v", err)
	}

	now = now.Add(2 * time.Minute) // 跨到 10-21 00:01
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("跨日后应重新有额度，实际 err=%v", err)
	}
	if n := f.callCount(); n != 2 {
		t.Fatalf("两天各应发出 1 条，实际 %d 次", n)
	}
}

// 账本不可用时必须 fail-closed：宁可少发一条，也不能把平台配额当无限量烧。
func TestSenderActiveBudgetFailsClosedWithoutStore(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 5,
	}, nil, time.Local)
	p := s.ForPlugin("digest")

	err := p.SendActive(context.Background(), host.SceneC2C, "u1",
		host.Reply{Image: host.ImageFromBytes(tinyPNG())})
	if !errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("没有账本时应按额度不可用拒绝，实际 err=%v", err)
	}
	if n := f.callCount(); n != 0 {
		t.Fatalf("账本不可用时不该发 HTTP，实际 %d 次", n)
	}
	if n := f.uploadCount(); n != 0 {
		t.Fatalf("账本不可用时不该上传，实际上传 %d 次", n)
	}
}

// 库已经关掉时同样拒发——这是"记账失败"最现实的一种形态。
func TestSenderActiveBudgetFailsClosedWhenStoreBroken(t *testing.T) {
	db := newUsageStore(t)
	if err := db.Close(); err != nil {
		t.Fatalf("关库: %v", err)
	}
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 5,
	}, db, time.Local)

	err := s.ForPlugin("digest").SendActive(context.Background(), host.SceneC2C, "u1",
		host.Reply{Content: "x"})
	if !errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("库不可用时应拒绝，实际 err=%v", err)
	}
	// 账本故障是运维问题，但错误必须指得回是哪个会话说起的（规则 7）。
	if !strings.Contains(err.Error(), "c2c") || !strings.Contains(err.Error(), "u1") {
		t.Fatalf("账本故障的错误缺少场景/peer: %v", err)
	}
	if n := f.callCount(); n != 0 {
		t.Fatalf("库不可用时不该发 HTTP，实际 %d 次", n)
	}
}

// 日预算落库的全部意义：进程重启（关库、重开、换一个 Sender）不能清零，
// 否则"重启循环不会烧额度"这句注释仍然是假的。
func TestSenderActiveBudgetSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	open := func() *store.DB {
		db, err := store.Open(path)
		if err != nil {
			t.Fatalf("打开测试库: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Migrate(context.Background()); err != nil {
			t.Fatalf("迁移: %v", err)
		}
		return db
	}
	policy := ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"bootnotice": true},
		DailyPerPeer: 2,
	}
	ctx := context.Background()

	first := open()
	f := &fakeAPI{}
	p := NewSender(f, slog.New(slog.DiscardHandler), policy, first, time.Local).ForPlugin("bootnotice")
	for i := 0; i < 2; i++ {
		if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err != nil {
			t.Fatalf("第 %d 次不应失败: %v", i+1, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("关库: %v", err)
	}

	second := open()
	f2 := &fakeAPI{}
	p2 := NewSender(f2, slog.New(slog.DiscardHandler), policy, second, time.Local).ForPlugin("bootnotice")
	if err := p2.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("重启后当日额度应仍是用尽的，实际 err=%v", err)
	}
	if n := f2.callCount(); n != 0 {
		t.Fatalf("额度已尽时不应发 HTTP，实际 %d 次", n)
	}
}

// 429 是平台在说"到点再来"，不是"可以重试"。核心要在 (scene, peer) 上记
// 同样的窗口：冷却期内直接失败、不出网，连换个 msg_id 也不放行（惩罚是
// 会话级的，不是某条消息的）。
func TestSenderRateLimitCooldownSkipsNetwork(t *testing.T) {
	f := &fakeAPI{rateLimitedAt: 1, retryAfter: 2 * time.Second}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	ctx := context.Background()

	ev := &host.Event{Scene: host.SceneGroup, PeerID: "g1", MsgID: "m1"}
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, host.ErrRateLimited) {
		t.Fatalf("429 应返回 host.ErrRateLimited，实际 %v", err)
	}

	next := &host.Event{Scene: host.SceneGroup, PeerID: "g1", MsgID: "m2"}
	start := time.Now()
	err := s.Reply(ctx, next, host.Reply{Content: "hi"})
	if !errors.Is(err, host.ErrRateLimited) {
		t.Fatalf("冷却期内应直接返回 host.ErrRateLimited，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("冷却期内应立刻失败，实际耗时 %s", elapsed)
	}
	if n := f.callCount(); n != 1 {
		t.Fatalf("冷却期内不得出网，实际调用 %d 次", n)
	}

	now = now.Add(3 * time.Second)
	if err := s.Reply(ctx, next, host.Reply{Content: "hi"}); err != nil {
		t.Fatalf("冷却结束后应能发送: %v", err)
	}
	if n := f.callCount(); n != 2 {
		t.Fatalf("冷却结束后应再发 1 次，实际共 %d 次", n)
	}
}

// 平台没给 Retry-After 时按 60s：惩罚窗口通常 ≥1 分钟，本地的缺省值不能是 0。
func TestSenderRateLimitCooldownDefaultsTo60s(t *testing.T) {
	f := &fakeAPI{rateLimitedAt: 1} // retryAfter 为 0：平台没带头
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.Local)
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	ctx := context.Background()
	ev := &host.Event{Scene: host.SceneC2C, PeerID: "u1", MsgID: "m1"}

	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, host.ErrRateLimited) {
		t.Fatalf("429 应返回 host.ErrRateLimited，实际 %v", err)
	}
	now = now.Add(59 * time.Second)
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, host.ErrRateLimited) {
		t.Fatalf("59s 时仍在缺省冷却窗口内，实际 %v", err)
	}
	now = now.Add(2 * time.Second)
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
		t.Fatalf("61s 时应已过冷却: %v", err)
	}
}

// 主动消息的冷却检查必须在扣日预算之前：冷却期内被挡下的请求既不出网，
// 也不该烧掉一格（预算按天计，比令牌贵得多）。
func TestSenderActiveRateLimitCooldownKeepsBudget(t *testing.T) {
	f := &fakeAPI{rateLimitedAt: 1, retryAfter: 2 * time.Second}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"bootnotice": true},
		DailyPerPeer: 2,
	}, newUsageStore(t), time.Local)
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	p := s.ForPlugin("bootnotice")
	ctx := context.Background()

	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrRateLimited) {
		t.Fatalf("429 应返回 host.ErrRateLimited，实际 %v", err)
	}
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrRateLimited) {
		t.Fatalf("冷却期内应返回 host.ErrRateLimited，实际 %v", err)
	}
	if n := f.callCount(); n != 1 {
		t.Fatalf("冷却期内不得出网，实际调用 %d 次", n)
	}

	now = now.Add(3 * time.Second)
	// 预算 2：被 429 拒掉的那条占 1 格，冷却期内被挡下的两次都不该再扣。
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("冷却结束后仍应有额度: %v", err)
	}
	if n := f.callCount(); n != 2 {
		t.Fatalf("应发 2 次（1 次 429 + 1 次成功），实际 %d 次", n)
	}
}

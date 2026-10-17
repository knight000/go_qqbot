package core

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/store"
)

// syncBuffer 让测试在读输出时不必担心调度协程正在写。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type replyPlugin struct{ fakePlugin }

func (p *replyPlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return &host.Reply{Content: "pong"}, nil
}

// 端到端：一条消息走完 收到 → 命中插件 → 返回回复 → 实际发送，四段都要出现在轨迹里。
func TestTraceEndToEnd(t *testing.T) {
	out := &syncBuffer{}
	tr := NewTrace(out, LevelFull)
	log := slog.New(slog.DiscardHandler)

	db, err := store.Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}

	plug := &replyPlugin{fakePlugin{m: manifest("echo", 100, "!ping")}}
	router, err := NewRouter([]host.Plugin{plug})
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	f := &fakeAPI{}
	sender := NewSender(f, log, ActivePolicy{})
	sender.trace = tr
	disp := NewDispatcher(router, sender, log, time.Second)
	disp.trace = tr

	c := &Core{store: db, log: log, trace: tr, sender: sender, router: router, dispatch: disp}
	c.Handle(context.Background(), &host.Event{
		ID:       "e1",
		Kind:     host.KindMessage,
		Name:     "GROUP_AT_MESSAGE_CREATE",
		Scene:    host.SceneGroup,
		PeerID:   "g1",
		SenderID: "u1",
		MsgID:    "e1",
		Content:  "!ping",
	})

	waitFor(t, out, "已发送")
	got := out.String()
	for _, want := range []string{
		"收到 群聊消息 · GROUP_AT_MESSAGE_CREATE",
		"发送者：u1",
		"peer  ：g1",
		"内容  ：!ping",
		`命中插件 echo · 命令 "!ping" · 优先级 100`,
		"插件 echo 返回回复",
		"已发送（被动回复 群聊 → g1）",
		"seq=1",
		"内容  ：pong",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("轨迹缺少 %q，实际输出:\n%s", want, got)
		}
	}
}

// 关闭调试时 trace 为 nil，所有方法必须安全可调用。
// plugin 级别：只留"触发插件的内容 + 回复正文"。
// 端到端跑一遍，确认事件 ID、union、发送者、peer、seq 全部不出现。
func TestTracePluginLevel(t *testing.T) {
	out := &syncBuffer{}
	tr := NewTrace(out, LevelPlugin)
	log := slog.New(slog.DiscardHandler)

	db, err := store.Open(filepath.Join(t.TempDir(), "trace-plugin.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}

	plug := &replyPlugin{fakePlugin{m: manifest("echo", 100, "!ping")}}
	router, err := NewRouter([]host.Plugin{plug})
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	f := &fakeAPI{}
	sender := NewSender(f, log, ActivePolicy{})
	sender.trace = tr
	disp := NewDispatcher(router, sender, log, time.Second)
	disp.trace = tr

	c := &Core{store: db, log: log, trace: tr, sender: sender, router: router, dispatch: disp}
	c.Handle(context.Background(), &host.Event{
		ID:       "EVENT-ID-123",
		Kind:     host.KindMessage,
		Name:     "GROUP_AT_MESSAGE_CREATE",
		Scene:    host.SceneGroup,
		PeerID:   "GROUP-OPENID-999",
		SenderID: "SENDER-OPENID-888",
		UnionID:  "UNION-XYZ",
		MsgID:    "EVENT-ID-123",
		Content:  "!ping",
	})

	waitFor(t, out, "内容  ：pong")
	got := out.String()

	for _, want := range []string{
		`命中插件 echo · 命令 "!ping"`,
		"内容  ：!ping",
		"被动回复 群聊",
		"内容  ：pong",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plugin 级别缺少 %q，实际输出:\n%s", want, got)
		}
	}
	for _, bad := range []string{
		"收到 群聊",             // 全量入站不下发
		"UNION-XYZ",         // 不打印 union
		"EVENT-ID-123",      // 不打印事件 ID
		"SENDER-OPENID-888", // 不打印发送者
		"GROUP-OPENID-999",  // 不打印 peer
		"seq=",
		"优先级",
		"插件 echo 返回回复",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("plugin 级别不应出现 %q，实际输出:\n%s", bad, got)
		}
	}
}

// plugin 级别下未命中、重复、原始 JSON 一律安静：它们是全量级别的信息。
func TestTracePluginLevelSkipsNoise(t *testing.T) {
	out := &syncBuffer{}
	tr := NewTrace(out, LevelPlugin)
	ev := &host.Event{
		ID: "e1", Kind: host.KindMessage, Name: "C2C_MESSAGE_CREATE", Scene: host.SceneC2C,
		PeerID: "u1", SenderID: "u1", UnionID: "union-1", Content: "随便聊聊",
	}
	tr.Received(ev)
	tr.Duplicate(ev)
	tr.NoMatch(ev)
	tr.Raw("C2C_MESSAGE_CREATE", []byte(`{"union_openid":"union-1"}`))

	if got := out.String(); got != "" {
		t.Fatalf("plugin 级别不应输出任何内容，实际: %q", got)
	}
}

func TestNilTraceIsSafe(t *testing.T) {
	var tr *Trace
	ev := &host.Event{ID: "e", Kind: host.KindMessage, Name: "C2C_MESSAGE_CREATE", Scene: host.SceneC2C}
	tr.Received(ev)
	tr.Duplicate(ev)
	tr.NoMatch(ev)
	tr.Hit(Match{}, nil)
	tr.PluginDone(Match{}, 0, nil, nil)
	tr.CoreCommand("/admin")
	tr.Sent("被动回复", host.SceneC2C, "u", 1, "x", nil, 0)
	tr.Raw("C2C_MESSAGE_CREATE", []byte(`{}`))
}

// 重复事件也要留痕：调试时"为什么没回复"往往是去重表拦下了。
func TestTraceDuplicate(t *testing.T) {
	out := &syncBuffer{}
	tr := NewTrace(out, LevelFull)
	tr.Duplicate(&host.Event{ID: "e1"})
	if got := out.String(); !strings.Contains(got, "重复事件，已跳过：e1") {
		t.Fatalf("重复事件未被标记，实际: %q", got)
	}
}

func TestTraceRaw(t *testing.T) {
	out := &syncBuffer{}
	tr := NewTrace(out, LevelFull)
	tr.Raw("GROUP_AT_MESSAGE_CREATE", []byte(`{"author":{"member_openid":"abc"}}`))
	got := out.String()
	for _, want := range []string{"原始事件 GROUP_AT_MESSAGE_CREATE", `"member_openid": "abc"`} {
		if !strings.Contains(got, want) {
			t.Errorf("原始事件输出缺少 %q:\n%s", want, got)
		}
	}
}

// 路由要告诉调试输出"命中的是哪条命令"，否则多命令插件看不出原因。
func TestRouterMatchReportsCommand(t *testing.T) {
	m := manifest("multi", 0, "")
	m.Commands = []host.Command{
		{Prefix: "!a", Priority: 10},
		{Prefix: "!b", Priority: 100},
	}
	r, err := NewRouter([]host.Plugin{&fakePlugin{m: m}})
	if err != nil {
		t.Fatal(err)
	}
	ms := r.Match(&host.Event{Kind: host.KindMessage, Scene: host.SceneGroup, Content: "!b x"})
	if len(ms) != 1 || ms[0].Command != "!b" || ms[0].Priority != 100 {
		t.Fatalf("应命中 !b(100)，实际 %+v", ms)
	}
}

func waitFor(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待轨迹中的 %q 超时，当前输出:\n%s", want, out.String())
}

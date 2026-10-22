package core

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

// timeStampRe 锁住时间戳的粒度：到秒，不带毫秒。
var timeStampRe = regexp.MustCompile(`^\[\d{2}:\d{2}:\d{2}\] `)

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
	sender := NewSender(f, log, ActivePolicy{}, nil, time.Local)
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
		Username: "小明",
		MsgID:    "e1",
		Content:  "!ping",
		Actor:    host.Principal{GroupRole: host.GroupRoleOwner},
	})

	waitFor(t, out, "已发送")
	got := out.String()
	for _, want := range []string{
		"收到 群聊消息 · GROUP_AT_MESSAGE_CREATE",
		"群    ：(g1)",              // 本地没登记过名字，只留 openid
		"发送者：[user]小明(u1)[owner]", // admins 表为空，Bot 角色回落 user
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
	sender := NewSender(f, log, ActivePolicy{}, nil, time.Local)
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
	tr.Received(context.Background(), ev)
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
	tr.Received(context.Background(), ev)
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

// 被同组前序插件阻断的匹配者必须在 full 级轨迹里留痕：这是排查
// "zzz 明明声明了 yield=true，为什么没反应"的唯一出口（阻断不是错误，不进 slog）。
func TestTraceBlockedNamesBothPlugins(t *testing.T) {
	out := &syncBuffer{}
	tr := NewTrace(out, LevelFull)
	blocker := yieldStub("aaa", 100, false)
	skipped := yieldStub("zzz", 100, true)
	d, _ := newTestDispatcher(t, blocker, skipped)
	d.trace = tr

	d.handle(context.Background(), groupEvent("/cmd", host.Principal{Role: host.RoleUser}))

	got := out.String()
	for _, want := range []string{
		" 未执行 · 插件 zzz", // 被挡住的是谁
		`命令 "/cmd"`,     // 哪条命令命中了它
		"（aaa 未让位）",     // 谁挡的
	} {
		if !strings.Contains(got, want) {
			t.Errorf("轨迹缺少 %q，实际输出:\n%s", want, got)
		}
	}
}

// 被不让位者挡下的候选有两种，「没执行」的原因不同：有权限的是被挡住，
// 无权限的本就不会执行。后者必须记「权限不足」，否则排障会往让位链上找。
func TestTraceBlockedDoesNotMislabelDenied(t *testing.T) {
	out := &syncBuffer{}
	tr := NewTrace(out, LevelFull)
	blocker := yieldStub("mmm", 100, false)
	denied := newStub("zzz", "/cmd", host.PermBotAdmin, "")
	d, _ := newTestDispatcher(t, blocker, denied)
	d.trace = tr

	d.handle(context.Background(), groupEvent("/cmd", host.Principal{Role: host.RoleUser}))

	got := out.String()
	if !strings.Contains(got, "权限不足 · 插件 zzz") {
		t.Errorf("无权限的候选应记「权限不足」，实际输出:\n%s", got)
	}
	if strings.Contains(got, "未执行 · 插件 zzz") {
		t.Errorf("无权限的候选不该记「未让位」，实际输出:\n%s", got)
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

// 群聊的"收到"块：群名取自本地登记，发送者带昵称与群内角色，
// 多行正文对齐到正文列（标签占 8 列）。
func TestTraceReceivedGroupFormat(t *testing.T) {
	out := &syncBuffer{}
	tr := NewTrace(out, LevelFull)
	tr.SetGroupNames(func(_ context.Context, openid string) string {
		if openid == "G1" {
			return "测试群"
		}
		return ""
	})
	tr.Received(context.Background(), &host.Event{
		ID: "E1", Kind: host.KindMessage, Name: "GROUP_MESSAGE_CREATE",
		Scene: host.SceneGroup, PeerID: "G1", SenderID: "U1",
		Username: "小明", Content: "第一行\n第二行",
		Actor: host.Principal{GroupRole: host.GroupRoleAdmin},
	})

	got := out.String()
	want := "收到 群聊消息 · GROUP_MESSAGE_CREATE\n" +
		"群    ：测试群(G1)\n" +
		"发送者：[user]小明(U1)[admin]\n" +
		"peer  ：G1\n" +
		"事件  ：E1\n" +
		"内容  ：第一行\n        第二行\n"
	if !strings.Contains(got, want) {
		t.Fatalf("群聊轨迹格式不符，期望包含:\n%s\n实际:\n%s", want, got)
	}
	// 时间戳是 [HH:MM:SS]，不带毫秒
	if !timeStampRe.MatchString(got) {
		t.Fatalf("时间戳应形如 [00:30:40]，实际:\n%s", got)
	}
	// 测试用的 buffer 不是终端，不能混进转义码
	if strings.Contains(got, "\x1b") {
		t.Fatalf("非终端输出不应带 ANSI 转义:\n%q", got)
	}
}

// 单聊没有群名与群内角色：这两段应当整行/整段消失，而不是显示空值。
func TestTraceReceivedC2CFormat(t *testing.T) {
	out := &syncBuffer{}
	tr := NewTrace(out, LevelFull)
	tr.Received(context.Background(), &host.Event{
		ID: "E2", Kind: host.KindMessage, Name: "C2C_MESSAGE_CREATE",
		Scene: host.SceneC2C, PeerID: "U1", SenderID: "U1", Content: "你好",
	})

	got := out.String()
	want := "收到 单聊消息 · C2C_MESSAGE_CREATE\n" +
		"发送者：[user](U1)\n" + // 单聊不下发昵称，也没有 member_role
		"peer  ：U1\n" +
		"事件  ：E2\n" +
		"内容  ：你好\n"
	if !strings.Contains(got, want) {
		t.Fatalf("单聊轨迹格式不符，期望包含:\n%s\n实际:\n%s", want, got)
	}
	if strings.Contains(got, "群    ：") {
		t.Fatalf("单聊不应出现群名行:\n%s", got)
	}
}

// Bot 角色必须出现在"收到"块里：它由 resolveActor 查 admins 表，
// 而"收到"打印在去重与群闸门之前。回归点——若这次提前解析被挪回去，
// 管理员的消息会显示成 [user]。
func TestTraceReceivedShowsBotRole(t *testing.T) {
	out := &syncBuffer{}
	tr := NewTrace(out, LevelFull)
	log := slog.New(slog.DiscardHandler)

	db, err := store.Open(filepath.Join(t.TempDir(), "trace-role.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	if err := db.UpsertAdmin(context.Background(), store.Admin{
		OpenID: "ADMIN-1", Role: string(host.RoleAdmin), SourceScene: "group",
	}); err != nil {
		t.Fatalf("写入管理员: %v", err)
	}

	router, err := NewRouter([]host.Plugin{&replyPlugin{fakePlugin{m: manifest("echo", 100, "!ping")}}})
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	sender := NewSender(&fakeAPI{}, log, ActivePolicy{}, nil, time.Local)
	disp := NewDispatcher(router, sender, log, time.Second)
	c := &Core{store: db, log: log, trace: tr, sender: sender, router: router, dispatch: disp}

	c.Handle(context.Background(), &host.Event{
		ID:       "E-ROLE",
		Kind:     host.KindMessage,
		Name:     "GROUP_MESSAGE_CREATE",
		Scene:    host.SceneGroup,
		PeerID:   "G1",
		SenderID: "ADMIN-1",
		Username: "管理员",
		MsgID:    "E-ROLE",
		Content:  "普通文本，不命中任何指令",
	})

	got := out.String()
	// 这条消息没有 member_role，所以只该有 Bot 角色，不该出现空的 [—]。
	if want := "发送者：[admin]管理员(ADMIN-1)\n"; !strings.Contains(got, want) {
		t.Fatalf("发送者应带 Bot 角色，期望包含 %q，实际:\n%s", want, got)
	}
}

// 群名必须来自 qq_groups：接线断了的话"群"行会永远只剩 openid。
func TestNewWiresGroupNamesToTrace(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "trace-group.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	if err := db.SetGroupName(ctx, "G1", "记下来的群名", "manual"); err != nil {
		t.Fatalf("写群名: %v", err)
	}

	tr := NewTrace(&syncBuffer{}, LevelFull)
	if _, err := New(Options{Store: db, Config: &config.Config{}, Trace: tr}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := tr.groupLabel(ctx, "G1"); got != "记下来的群名(G1)" {
		t.Fatalf("群名未接到轨迹上，实际 %q", got)
	}
	if got := tr.groupLabel(ctx, "G2"); got != "(G2)" {
		t.Fatalf("未登记的群不应编名字，实际 %q", got)
	}
}

// 会话接管在 plugin 级也要有一行：这条路径没有命令命中，静态匹配那套轨迹
// 全都不适用，不给这行的话"谁触发了我"就没有任何答案。
func TestTraceSessionAtBothLevels(t *testing.T) {
	m := Match{Manifest: host.Manifest{Name: "md", Version: "0.1"}}
	ev := &host.Event{PeerID: "g1", Scene: host.SceneGroup}

	full := &syncBuffer{}
	NewTrace(full, LevelFull).Session(m, ev, 4*time.Minute+12*time.Second)
	got := full.String()
	for _, want := range []string{"会话接管", "md v0.1", "g1", "剩余 4 分 12 秒"} {
		if !strings.Contains(got, want) {
			t.Errorf("full 级应包含 %q，实际: %q", want, got)
		}
	}

	plugin := &syncBuffer{}
	NewTrace(plugin, LevelPlugin).Session(m, ev, time.Minute)
	got = plugin.String()
	if !strings.Contains(got, "会话接管 · 插件 md v0.1") {
		t.Errorf("plugin 级应有一行接管提示，实际: %q", got)
	}
	if strings.Contains(got, "g1") {
		t.Errorf("plugin 级不该带 peer，实际: %q", got)
	}
}

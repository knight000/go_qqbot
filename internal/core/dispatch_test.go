package core

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
)

// stubPlugin 记录被真正调用的次数——权限不足时核心不应该调用它。
type stubPlugin struct {
	manifest host.Manifest
	mu       sync.Mutex
	calls    int
}

func newStub(name, prefix string, perm host.Perm, deny string, extra ...host.Command) *stubPlugin {
	cmds := append([]host.Command{{
		Prefix: prefix, Priority: 100, Permission: perm, DenyReply: deny,
	}}, extra...)
	return &stubPlugin{manifest: host.Manifest{
		Name:       name,
		Version:    "0.0.1",
		APIVersion: host.APIVersion,
		Intents:    []string{"message"},
		Commands:   cmds,
	}}
}

func (s *stubPlugin) Manifest() host.Manifest               { return s.manifest }
func (s *stubPlugin) Init(context.Context, host.Host) error { return nil }
func (s *stubPlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return &host.Reply{Content: s.manifest.Name + ":ok"}, nil
}

func (s *stubPlugin) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newTestDispatcher(t *testing.T, plugins ...host.Plugin) (*Dispatcher, *fakeAPI) {
	t.Helper()
	router, err := NewRouter(plugins)
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	f := &fakeAPI{}
	log := slog.New(slog.DiscardHandler)
	d := NewDispatcher(router, NewSender(f, log, ActivePolicy{}, nil), log, time.Second)
	return d, f
}

func groupEvent(content string, p host.Principal) *host.Event {
	p.Scene = host.SceneGroup
	p.PeerID = "g1"
	if p.OpenID == "" {
		p.OpenID = "u1"
	}
	return &host.Event{
		ID: "e1", Kind: host.KindMessage, Name: "GROUP_MESSAGE_CREATE",
		Scene: host.SceneGroup, PeerID: "g1", SenderID: p.OpenID,
		MsgID: "m1", Content: content, Actor: p,
	}
}

// 权限不足的插件根本不该被调用，也不该产生任何回复（默认静默）。
func TestDispatchDeniesWithoutPermission(t *testing.T) {
	p := newStub("adminonly", "/secret", host.PermBotAdmin, "该指令仅管理员可用。")
	d, f := newTestDispatcher(t, p)

	d.handle(context.Background(), groupEvent("/secret", host.Principal{Role: host.RoleUser, GroupRole: host.GroupRoleOwner}))
	if p.callCount() != 0 {
		t.Fatal("普通用户（即使是平台群主）不应触发 bot-admin 插件")
	}
	if len(f.bodies) != 0 {
		t.Fatalf("默认应静默，实际发出 %d 条", len(f.bodies))
	}
}

// 打开全局开关后，拒绝语由核心发出——插件里没有一行判权代码。
func TestDispatchDenyReplyWhenEnabled(t *testing.T) {
	p := newStub("adminonly", "/secret", host.PermBotAdmin, "该指令仅管理员可用。")
	d, f := newTestDispatcher(t, p)
	d.denyReply = true

	d.handle(context.Background(), groupEvent("/secret", host.Principal{Role: host.RoleUser}))
	if p.callCount() != 0 {
		t.Fatal("插件不应被调用")
	}
	if len(f.bodies) != 1 || f.bodies[0].Content != "该指令仅管理员可用。" {
		t.Fatalf("应回复一条拒绝语，实际 %+v", f.bodies)
	}
}

// 管理员放行。
func TestDispatchAllowsBotAdmin(t *testing.T) {
	p := newStub("adminonly", "/secret", host.PermBotAdmin, "")
	d, f := newTestDispatcher(t, p)

	d.handle(context.Background(), groupEvent("/secret", host.Principal{Role: host.RoleAdmin, GroupRole: host.GroupRoleMember}))
	if p.callCount() != 1 {
		t.Fatalf("Bot 管理员应触发插件，实际 %d 次", p.callCount())
	}
	if len(f.bodies) != 1 || f.bodies[0].Content != "adminonly:ok" {
		t.Fatalf("插件回复应被发出，实际 %+v", f.bodies)
	}
}

// 群管权限只看平台角色：群主 / 群管理员满足，普通群成员不满足。
func TestDispatchGroupRolePermission(t *testing.T) {
	cases := []struct {
		role host.GroupRole
		want int
	}{
		{host.GroupRoleOwner, 1},
		{host.GroupRoleAdmin, 1},
		{host.GroupRoleMember, 0},
		{host.GroupRoleUnknown, 0},
	}
	for _, c := range cases {
		p := newStub("groupcmd", "/mute", host.PermGroupAdmin, "")
		d, _ := newTestDispatcher(t, p)
		d.handle(context.Background(), groupEvent("/mute", host.Principal{Role: host.RoleUser, GroupRole: c.role}))
		if p.callCount() != c.want {
			t.Errorf("群内角色 %q: 调用 %d 次，want %d", c.role, p.callCount(), c.want)
		}
	}
}

// 高优先级但无权限的插件不该挡住后面能处理它的插件。
func TestDispatchFallsThroughOnPermission(t *testing.T) {
	admin := newStub("adminonly", "/cmd", host.PermBotAdmin, "")
	admin.manifest.Commands[0].Priority = 200
	public := newStub("public", "/cmd", host.PermUser, "")
	public.manifest.Commands[0].Priority = 100

	d, f := newTestDispatcher(t, admin, public)
	d.handle(context.Background(), groupEvent("/cmd", host.Principal{Role: host.RoleUser}))
	if admin.callCount() != 0 {
		t.Fatal("无权限的高优先级插件不应执行")
	}
	if public.callCount() != 1 {
		t.Fatalf("低优先级的公开插件应接手，实际 %d 次", public.callCount())
	}
	if len(f.bodies) != 1 || f.bodies[0].Content != "public:ok" {
		t.Fatalf("应由 public 回复，实际 %+v", f.bodies)
	}
}

// 命令未声明权限时继承插件级默认。
func TestRouterInheritsManifestPermission(t *testing.T) {
	p := &stubPlugin{manifest: host.Manifest{
		Name:       "inherit",
		APIVersion: host.APIVersion,
		Intents:    []string{"message"},
		Permission: host.PermBotAdmin,
		DenyReply:  "插件级拒绝语",
		Commands: []host.Command{
			{Prefix: "/a", Priority: 10},
			{Prefix: "/b", Priority: 10, Permission: host.PermUser},
			{Prefix: "/c", Priority: 10, DenyReply: "命令级拒绝语"},
		},
	}}
	router, err := NewRouter([]host.Plugin{p})
	if err != nil {
		t.Fatal(err)
	}

	m := router.Match(groupEvent("/a", host.Principal{}))
	if len(m) != 1 || m[0].Perm != host.PermBotAdmin || m[0].DenyReply != "插件级拒绝语" {
		t.Fatalf("/a 应继承插件级权限与拒绝语: %+v", m)
	}
	m = router.Match(groupEvent("/b", host.Principal{}))
	if len(m) != 1 || m[0].Perm != host.PermUser {
		t.Fatalf("/b 应显式公开: %+v", m)
	}
	m = router.Match(groupEvent("/c", host.Principal{}))
	if len(m) != 1 || m[0].Perm != host.PermBotAdmin || m[0].DenyReply != "命令级拒绝语" {
		t.Fatalf("/c 应按命令级覆盖拒绝语、继承权限: %+v", m)
	}
}

// 权限位拼错必须在装配期报错，而不是线上表现为"谁都能用"。
func TestRouterRejectsUnknownPermission(t *testing.T) {
	bad := newStub("bad", "/x", host.Perm(1<<20), "")
	if _, err := NewRouter([]host.Plugin{bad}); err == nil {
		t.Fatal("未定义的权限位应拒绝加载")
	}
}

// yieldStub 是同 priority 多插件场景的夹具：名字决定组内顺序，Yield 决定是否让位。
func yieldStub(name string, priority int, yield bool) *stubPlugin {
	p := newStub(name, "/cmd", host.PermUser, "")
	p.manifest.Yield = yield
	p.manifest.Commands[0].Priority = priority
	return p
}

// 不让位者执行完就停在它：名字靠前的插件不能因为字典序而永久压制靠后的插件，
// 但这里的"压制"必须是它自己声明出来的（Yield=false），而不是中标名次的副作用。
func TestDispatchStopsAtNonYieldingPlugin(t *testing.T) {
	stop := yieldStub("aaa", 100, false)
	next := yieldStub("zzz", 100, true)
	d, _ := newTestDispatcher(t, stop, next)

	d.handle(context.Background(), groupEvent("/cmd", host.Principal{Role: host.RoleUser}))
	if stop.callCount() != 1 {
		t.Fatalf("aaa 应执行一次，实际 %d 次", stop.callCount())
	}
	if next.callCount() != 0 {
		t.Fatalf("aaa 未让位，zzz 不应执行，实际 %d 次", next.callCount())
	}
}

// 让位就是让位：前一个声明 Yield=true，组内后一个照常执行。
func TestDispatchContinuesWhileYielding(t *testing.T) {
	first := yieldStub("aaa", 100, true)
	last := yieldStub("zzz", 100, false)
	d, _ := newTestDispatcher(t, first, last)

	d.handle(context.Background(), groupEvent("/cmd", host.Principal{Role: host.RoleUser}))
	if first.callCount() != 1 || last.callCount() != 1 {
		t.Fatalf("两者都应执行，实际 aaa=%d zzz=%d", first.callCount(), last.callCount())
	}
}

// 执行到第一个不让位者为止，不继续向后：让位链是"逐个人声明"，不是"整组投票"。
func TestDispatchStopsAtFirstNonYielder(t *testing.T) {
	first := yieldStub("aaa", 100, true)
	middle := yieldStub("mmm", 100, false)
	last := yieldStub("zzz", 100, true)
	d, _ := newTestDispatcher(t, first, middle, last)

	d.handle(context.Background(), groupEvent("/cmd", host.Principal{Role: host.RoleUser}))
	if first.callCount() != 1 || middle.callCount() != 1 {
		t.Fatalf("aaa 与 mmm 应执行，实际 aaa=%d mmm=%d", first.callCount(), middle.callCount())
	}
	if last.callCount() != 0 {
		t.Fatalf("mmm 未让位，zzz 不应执行，实际 %d 次", last.callCount())
	}
}

// yield 只在同 priority 组内生效：高优先级让位不会把事件送给低优先级插件。
func TestDispatchYieldDoesNotCrossPriority(t *testing.T) {
	high := yieldStub("aaa", 200, true)
	low := yieldStub("zzz", 100, true)
	d, _ := newTestDispatcher(t, high, low)

	d.handle(context.Background(), groupEvent("/cmd", host.Principal{Role: host.RoleUser}))
	if high.callCount() != 1 {
		t.Fatalf("高优先级插件应执行，实际 %d 次", high.callCount())
	}
	if low.callCount() != 0 {
		t.Fatalf("跨优先级不扇出，低优先级插件不应执行，实际 %d 次", low.callCount())
	}
}

// 无权限的匹配者只是被跳过，不改变"组内是否继续"——它不该像不让位者那样截断后续。
func TestDispatchDeniedDoesNotBlockGroup(t *testing.T) {
	first := yieldStub("aaa", 100, true)
	denied := newStub("bbb", "/cmd", host.PermBotAdmin, "")
	last := yieldStub("zzz", 100, true)
	d, _ := newTestDispatcher(t, first, denied, last)

	d.handle(context.Background(), groupEvent("/cmd", host.Principal{Role: host.RoleUser}))
	if first.callCount() != 1 || last.callCount() != 1 {
		t.Fatalf("无权者前后两个插件都应执行，实际 aaa=%d zzz=%d", first.callCount(), last.callCount())
	}
	if denied.callCount() != 0 {
		t.Fatalf("无权限插件不应执行，实际 %d 次", denied.callCount())
	}
}

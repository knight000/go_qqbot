package adminping

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/host"
)

// fakeHost 只填本插件用到的能力（日志与配置）；鉴权已不在插件里，IsAdmin 不参与。
type fakeHost struct{ cfg host.Params }

func (f *fakeHost) Plugin() string              { return "adminping" }
func (f *fakeHost) Config() host.Params         { return f.cfg }
func (f *fakeHost) Sender() host.Sender         { return nil }
func (f *fakeHost) KV() host.KV                 { return nil }
func (f *fakeHost) PeerKV(string) host.KV       { return nil }
func (f *fakeHost) Query() host.QueryStore      { return nil }
func (f *fakeHost) HTTP() host.HTTP             { return nil }
func (f *fakeHost) Scheduler() host.Scheduler   { return nil }
func (f *fakeHost) Admins() host.AdminDirectory { return nil }
func (f *fakeHost) IsAdmin(context.Context, host.Identity) (bool, error) {
	return false, nil
}
func (f *fakeHost) Log() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := &Plugin{}
	if err := p.Init(context.Background(), &fakeHost{cfg: host.Params{}}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p
}

func ev(content string) *host.Event {
	return &host.Event{
		ID:       "e1",
		Kind:     host.KindMessage,
		Name:     "GROUP_AT_MESSAGE_CREATE",
		Scene:    host.SceneGroup,
		PeerID:   "group-1",
		SenderID: "user-1",
		MsgID:    "m1",
		Content:  content,
		Actor: host.Principal{
			OpenID: "user-1", Scene: host.SceneGroup, PeerID: "group-1",
			Role: host.RoleAdmin, GroupRole: host.GroupRoleMember,
		},
	}
}

func TestIsPing(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"/ping", true},
		{"  /ping  ", true},
		{"/PING", true},
		{"/ping 忽略多余参数", true},
		{"/pingpong", false},
		{"!ping", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isPing(c.in); got != c.want {
			t.Errorf("isPing(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// 鉴权必须是声明式的：插件不判权，能力边界写进 Manifest。
func TestManifestRequiresBotAdmin(t *testing.T) {
	m := (&Plugin{}).Manifest()
	if len(m.Commands) != 1 {
		t.Fatalf("Commands = %+v, want 单个 /ping", m.Commands)
	}
	cmd := m.Commands[0]
	if cmd.Permission != host.PermBotAdmin {
		t.Fatalf("Permission = %s, want bot-admin", cmd.Permission)
	}
	if cmd.DenyReply == "" {
		t.Fatal("DenyReply 不应为空：全局开关打开时要有话可说")
	}
	if !cmd.Permission.Admits(host.Principal{Role: host.RoleAdmin}) {
		t.Fatal("Bot 管理员应被允许")
	}
	if !cmd.Permission.Admits(host.Principal{Role: host.RoleOwner}) {
		t.Fatal("Bot 所有者应被允许")
	}
	if cmd.Permission.Admits(host.Principal{Role: host.RoleUser}) {
		t.Fatal("普通用户不应被允许")
	}
	// 平台群主不是 Bot 管理员：群主身份不满足 bot-admin
	if cmd.Permission.Admits(host.Principal{Role: host.RoleUser, GroupRole: host.GroupRoleOwner, Scene: host.SceneGroup}) {
		t.Fatal("平台群主不应满足 bot-admin")
	}
}

func TestOtherCommandsIgnored(t *testing.T) {
	p := newPlugin(t)
	for _, c := range []string{"hello", "/reset", "/pingpong"} {
		reply, err := p.Handle(context.Background(), ev(c))
		if err != nil {
			t.Fatalf("Handle(%q): %v", c, err)
		}
		if reply != nil {
			t.Errorf("%q 不应被本插件处理", c)
		}
	}
}

func TestAdminGetsFullReport(t *testing.T) {
	p := newPlugin(t)
	reply, err := p.Handle(context.Background(), ev("/ping"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply == nil {
		t.Fatal("管理员应收到回复")
	}
	for _, want := range []string{"/pong", "版本", "运行", "CPU", "内存", "磁盘", "目录"} {
		if !strings.Contains(reply.Content, want) {
			t.Errorf("报告缺少 %q:\n%s", want, reply.Content)
		}
	}
	if reply.MsgType != 0 {
		t.Errorf("MsgType = %d, want 0（文本）", reply.MsgType)
	}
	t.Logf("报告内容:\n%s", reply.Content)
}

func TestManifestDeclaresPing(t *testing.T) {
	m := (&Plugin{}).Manifest()
	if m.Name != "adminping" || m.APIVersion != host.APIVersion {
		t.Fatalf("Manifest 不正确: %+v", m)
	}
	if len(m.Commands) != 1 || m.Commands[0].Prefix != "/ping" {
		t.Fatalf("Commands = %+v, want 单个 /ping", m.Commands)
	}
	// /ping 不能被核心保留前缀拦截
	for _, rp := range []string{"/claim", "/reset", "/grant", "/revoke"} {
		if strings.HasPrefix(strings.ToLower(m.Commands[0].Prefix), rp) {
			t.Fatalf("命令 %q 撞上保留前缀 %q", m.Commands[0].Prefix, rp)
		}
	}
}

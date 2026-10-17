package adminping

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/knight000/qqbot/internal/host"
)

type fakeHost struct {
	admin bool
	err   error
	cfg   host.Params
}

func (f *fakeHost) Plugin() string            { return "adminping" }
func (f *fakeHost) Config() host.Params       { return f.cfg }
func (f *fakeHost) Sender() host.Sender       { return nil }
func (f *fakeHost) KV() host.KV               { return nil }
func (f *fakeHost) Query() host.QueryStore    { return nil }
func (f *fakeHost) HTTP() host.HTTP           { return nil }
func (f *fakeHost) Scheduler() host.Scheduler { return nil }
func (f *fakeHost) Log() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (f *fakeHost) IsAdmin(ctx context.Context, scene host.Scene, openid, peer string) (bool, error) {
	return f.admin, f.err
}

func newPlugin(t *testing.T, admin bool, err error, cfg host.Params) *Plugin {
	t.Helper()
	if cfg == nil {
		cfg = host.Params{}
	}
	p := &Plugin{}
	if e := p.Init(context.Background(), &fakeHost{admin: admin, err: err, cfg: cfg}); e != nil {
		t.Fatalf("Init: %v", e)
	}
	return p
}

func ev(content string) *host.Event {
	return &host.Event{
		ID:       "e1",
		Type:     host.EventGroupMessage,
		Scene:    host.SceneGroup,
		PeerID:   "group-1",
		SenderID: "user-1",
		MsgID:    "m1",
		Content:  content,
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

func TestNonAdminGetsNoReply(t *testing.T) {
	// 默认配置（未声明 silent）必须是完全静默
	p := newPlugin(t, false, nil, nil)
	reply, err := p.Handle(context.Background(), ev("/ping"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply != nil {
		t.Fatalf("非管理员不应收到回复，得到: %q", reply.Content)
	}
	if !p.silent {
		t.Fatal("默认 silent 应为 true")
	}
}

// 关掉静默后，非管理员只拿到一句拒绝，拿不到任何指标。
func TestNonAdminGetsDenyWhenNotSilent(t *testing.T) {
	p := newPlugin(t, false, nil, host.Params{"silent": "false"})
	reply, err := p.Handle(context.Background(), ev("/ping"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply == nil {
		t.Fatal("silent=false 时应回复")
	}
	if !strings.Contains(reply.Content, "管理员") {
		t.Errorf("拒绝语不对: %q", reply.Content)
	}
	for _, leak := range []string{"CPU", "内存", "磁盘", "目录"} {
		if strings.Contains(reply.Content, leak) {
			t.Errorf("拒绝语泄露了指标 %q: %q", leak, reply.Content)
		}
	}
}

// 权限后端故障时必须 fail-closed：静默模式不回任何内容，避免把主机信息漏给陌生人。
func TestAdminCheckErrorIsSilent(t *testing.T) {
	p := newPlugin(t, true, errors.New("db down"), nil)
	reply, err := p.Handle(context.Background(), ev("/ping"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply != nil {
		t.Fatalf("鉴权失败不应回复，得到: %q", reply.Content)
	}
}

func TestAdminCheckErrorNotSilent(t *testing.T) {
	p := newPlugin(t, true, errors.New("db down"), host.Params{"silent": false})
	reply, err := p.Handle(context.Background(), ev("/ping"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply == nil || !strings.Contains(reply.Content, "权限") {
		t.Fatalf("应回复权限查询失败，得到: %+v", reply)
	}
	if strings.Contains(reply.Content, "CPU") {
		t.Errorf("鉴权失败不应带指标: %q", reply.Content)
	}
}

func TestOtherCommandsIgnored(t *testing.T) {
	p := newPlugin(t, true, nil, nil)
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
	p := newPlugin(t, true, nil, host.Params{"silent": true})
	reply, err := p.Handle(context.Background(), ev("/ping"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply == nil {
		t.Fatal("管理员应收到回复")
	}
	for _, want := range []string{"/pong", "运行", "CPU", "内存", "磁盘", "目录"} {
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

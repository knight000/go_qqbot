package bootnotice

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/buildinfo"
	"github.com/knight000/go_qqbot/internal/host"
)

type sentMsg struct {
	scene host.Scene
	peer  string
	text  string
}

type fakeSender struct {
	msgs []sentMsg
	fail int // 前 N 次失败，用来验证重试
}

func (f *fakeSender) Reply(context.Context, *host.Event, host.Reply) error { return nil }

func (f *fakeSender) SendActive(_ context.Context, scene host.Scene, peer string, msg host.Reply) error {
	if f.fail > 0 {
		f.fail--
		return errors.New("模拟失败")
	}
	f.msgs = append(f.msgs, sentMsg{scene: scene, peer: peer, text: msg.Content})
	return nil
}

type fakeKV struct{ m map[string][]byte }

func (k *fakeKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := k.m[key]
	return v, ok, nil
}

func (k *fakeKV) Set(_ context.Context, key string, val []byte, _ time.Duration) error {
	k.m[key] = val
	return nil
}

func (k *fakeKV) Delete(_ context.Context, key string) error {
	delete(k.m, key)
	return nil
}

type fakeDir []host.AdminTarget

func (d fakeDir) Targets(context.Context) ([]host.AdminTarget, error) { return d, nil }

type fakeHost struct {
	cfg    host.Params
	sender *fakeSender
	kv     *fakeKV
	dir    fakeDir
}

func (f *fakeHost) Plugin() string              { return "bootnotice" }
func (f *fakeHost) Config() host.Params         { return f.cfg }
func (f *fakeHost) Sender() host.Sender         { return f.sender }
func (f *fakeHost) KV() host.KV                 { return f.kv }
func (f *fakeHost) PeerKV(string) host.KV       { return f.kv }
func (f *fakeHost) Query() host.QueryStore      { return nil }
func (f *fakeHost) HTTP() host.HTTP             { return nil }
func (f *fakeHost) Scheduler() host.Scheduler   { return nil }
func (f *fakeHost) Admins() host.AdminDirectory { return f.dir }
func (f *fakeHost) Renderer() host.Renderer     { return nil }
func (f *fakeHost) Session() host.Session       { return nil }
func (f *fakeHost) IsAdmin(context.Context, host.Identity) (bool, error) {
	return false, nil
}
func (f *fakeHost) Log() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newPlugin(t *testing.T, cfg host.Params, dir fakeDir) (*Plugin, *fakeHost) {
	t.Helper()
	h := &fakeHost{
		cfg:    cfg,
		sender: &fakeSender{},
		kv:     &fakeKV{m: map[string][]byte{}},
		dir:    dir,
	}
	p := &Plugin{}
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p, h
}

func TestComposeContainsStartAndPlugins(t *testing.T) {
	start := time.Date(2026, 10, 17, 15, 4, 5, 0, time.Local)
	prev := start.Add(-2*time.Hour - 13*time.Minute)
	text := compose(start, prev, true)

	for _, want := range []string{"机器人已上线", "10-17 15:04:05", "上次启动", "2小时13分", "启用插件", "bootnotice"} {
		if !strings.Contains(text, want) {
			t.Errorf("通知缺少 %q:\n%s", want, text)
		}
	}
	// 没有上次记录时不应出现"上次启动"
	if got := compose(start, time.Time{}, false); strings.Contains(got, "上次启动") {
		t.Errorf("首次启动不应显示上次启动:\n%s", got)
	}
	t.Logf("通知正文:\n%s", text)
}

func TestNotifySendsToReachableAdminsOnly(t *testing.T) {
	p, h := newPlugin(t, host.Params{}, fakeDir{
		{Role: "owner", C2COpenID: "c2c-owner"},
		{Role: "admin", C2COpenID: ""}, // 只有群身份，无法私聊
	})
	p.delay = 0
	p.run(context.Background())

	if len(h.sender.msgs) != 1 {
		t.Fatalf("应只给可私聊的 1 人发送，实际 %d", len(h.sender.msgs))
	}
	if h.sender.msgs[0].scene != host.SceneC2C || h.sender.msgs[0].peer != "c2c-owner" {
		t.Fatalf("收件人不对: %+v", h.sender.msgs[0])
	}
	if !strings.Contains(h.sender.msgs[0].text, "启用插件") {
		t.Errorf("正文缺少插件清单:\n%s", h.sender.msgs[0].text)
	}
	if _, ok := h.kv.m[keyLastStart]; !ok {
		t.Error("应记录本次启动时间")
	}
}

func TestNotifySkipsWhenNoReachableAdmin(t *testing.T) {
	p, h := newPlugin(t, host.Params{}, fakeDir{{Role: "owner", C2COpenID: ""}})
	p.delay = 0
	p.run(context.Background())

	if len(h.sender.msgs) != 0 {
		t.Fatalf("没有单聊身份时不应发送，实际 %d 条", len(h.sender.msgs))
	}
}

// 崩溃重启时不得反复刷屏：距上次启动不足 min_interval 整轮跳过。
func TestNotifySuppressedWithinMinInterval(t *testing.T) {
	p, h := newPlugin(t, host.Params{}, fakeDir{{Role: "owner", C2COpenID: "c2c-owner"}})
	p.delay = 0
	h.kv.m[keyLastStart] = []byte(time.Now().Add(-time.Minute).Format(time.RFC3339))

	p.run(context.Background())
	if len(h.sender.msgs) != 0 {
		t.Fatalf("间隔不足时不应发送，实际 %d 条", len(h.sender.msgs))
	}

	// 超过间隔后应正常发送
	h.kv.m[keyLastStart] = []byte(time.Now().Add(-time.Hour).Format(time.RFC3339))
	p.run(context.Background())
	if len(h.sender.msgs) != 1 {
		t.Fatalf("超过间隔后应发送，实际 %d 条", len(h.sender.msgs))
	}
	if !strings.Contains(h.sender.msgs[0].text, "上次启动") {
		t.Errorf("第二次启动应带上次启动时间:\n%s", h.sender.msgs[0].text)
	}
}

// 发送失败重试一次，仍失败则放弃，并把失败如实留在日志里。
func TestNotifyRetriesOnce(t *testing.T) {
	p, h := newPlugin(t, host.Params{}, fakeDir{{Role: "owner", C2COpenID: "c2c-owner"}})
	p.delay = 0
	p.retryDelay = time.Millisecond // 0 表示不重试，这里要测重试所以给个极小值
	p.minInterval = 0               // 用例内会连着跑两次，关掉间隔抑制
	h.sender.fail = 2

	p.run(context.Background())
	if len(h.sender.msgs) != 0 {
		t.Fatalf("两次都失败时不应记录成功发送，实际 %d", len(h.sender.msgs))
	}

	h.sender.fail = 1
	p.run(context.Background())
	if len(h.sender.msgs) != 1 {
		t.Fatalf("重试应成功，实际 %d 条", len(h.sender.msgs))
	}
}

// /reload 是管理员显式重启，必须发通知：否则"重启后确认更新是否生效"没有回执。
func TestNotifyIgnoresIntervalAfterReload(t *testing.T) {
	t.Setenv(buildinfo.RestartEnv, "1")
	p, h := newPlugin(t, host.Params{}, fakeDir{{Role: "owner", C2COpenID: "c2c-owner"}})
	p.delay = 0
	// 落在 min_interval 窗口内
	h.kv.m[keyLastStart] = []byte(time.Now().Add(-time.Minute).Format(time.RFC3339))

	p.run(context.Background())

	if len(h.sender.msgs) != 1 {
		t.Fatalf("/reload 重启应无视 min_interval，实际 %d 条", len(h.sender.msgs))
	}
	if !strings.Contains(h.sender.msgs[0].text, "/reload 重启") {
		t.Errorf("正文应标注 /reload 重启:\n%s", h.sender.msgs[0].text)
	}
	if !strings.Contains(h.sender.msgs[0].text, "版本") {
		t.Errorf("正文应带版本，便于确认是否换成了新构建:\n%s", h.sender.msgs[0].text)
	}
}

func TestDisabledPluginDoesNotStart(t *testing.T) {
	p, h := newPlugin(t, host.NewParams("bootnotice", map[string]any{"enabled": "false"}), fakeDir{{Role: "owner", C2COpenID: "c2c-owner"}})
	if p.enabled {
		t.Fatal("enabled=false 应关闭插件")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(h.sender.msgs) != 0 {
		t.Fatalf("关闭后不应发送，实际 %d", len(h.sender.msgs))
	}
}

func TestManifestDeclaresNoCommands(t *testing.T) {
	m := (&Plugin{}).Manifest()
	if m.Name != "bootnotice" || m.APIVersion != host.APIVersion {
		t.Fatalf("Manifest 不正确: %+v", m)
	}
	if len(m.Commands) != 0 || len(m.Intents) != 0 {
		t.Fatalf("bootnotice 不应订阅事件或声明命令: %+v", m)
	}
}

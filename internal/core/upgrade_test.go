package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/internal/upgrade"
	"github.com/knight000/go_qqbot/pkg/host"
)

// fakeUpgrade 是上层的替身：不发网络、不碰二进制，只负责"被调用了没有"
// 与"返回什么"。core 的测试从此只验证命令面的行为。
type fakeUpgrade struct {
	report     upgrade.Report
	previewErr error
	plan       *upgrade.Plan
	applyErr   error
	status     upgrade.Status
	statusErr  error

	previewCalls int
	applyCalls   int
	statusCalls  int
}

func (f *fakeUpgrade) Preview(context.Context) (upgrade.Report, error) {
	f.previewCalls++
	return f.report, f.previewErr
}

func (f *fakeUpgrade) Apply(context.Context) (*upgrade.Plan, error) {
	f.applyCalls++
	return f.plan, f.applyErr
}

func (f *fakeUpgrade) Status(context.Context) (upgrade.Status, error) {
	f.statusCalls++
	return f.status, f.statusErr
}

func newUpgradeCore(t *testing.T, svc UpgradeService) (*Core, *fakeAPI) {
	t.Helper()
	c, f := newAdminCore(t)
	c.upgrade = svc
	c.cfg = &config.Config{}
	return c, f
}

// 升级通道就是远程代码执行通道：辅助管理员不该有这个能力（决策 2）。
func TestUpgradeIsOwnerOnly(t *testing.T) {
	svc := &fakeUpgrade{report: upgrade.Report{Latest: "9.9.9"}}
	c, f := newUpgradeCore(t, svc)
	seedAdmin(t, c, "a1", "", store.RoleAdmin)

	for _, content := range []string{"/upgrade", "/upgrade apply", "/upgrade status"} {
		c.handleReserved(context.Background(), adminEvent(content, "a1", "", host.SceneC2C, "a1"))
		if got := lastReply(t, f); !strings.Contains(got, "仅所有者") {
			t.Errorf("%s 对管理员应被拒绝，实际: %q", content, got)
		}
	}
	if svc.previewCalls+svc.applyCalls+svc.statusCalls != 0 {
		t.Error("拒绝时不该调用任何升级能力")
	}
	if n := len(c.restart); n != 0 {
		t.Fatalf("拒绝时不该入队重启，实际 %d", n)
	}
}

// 预览只回报告正文，不入队、不改任何东西。
func TestUpgradePreviewRepliesReport(t *testing.T) {
	svc := &fakeUpgrade{report: upgrade.Report{
		Current: "1.2.3", Latest: "1.2.4", Tag: "v1.2.4",
		CoreName: "qqbot-1.2.4-windows-amd64.exe", CoreSize: 40 << 20,
	}}
	c, f := newUpgradeCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	if !c.handleReserved(context.Background(), adminEvent("/upgrade", "o1", "", host.SceneC2C, "o1")) {
		t.Fatal("/upgrade 必须被核心消费，不能落到插件")
	}
	got := lastReply(t, f)
	for _, want := range []string{"自更新预览", "1.2.4", "qqbot-1.2.4-windows-amd64.exe", "40.0 MiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("预览正文缺少 %q:\n%s", want, got)
		}
	}
	if n := len(c.restart); n != 0 {
		t.Fatalf("预览不该触发重启，实际 %d", n)
	}
}

func TestUpgradePreviewErrorIsReported(t *testing.T) {
	svc := &fakeUpgrade{previewErr: errors.New("请求 https://x 返回 404")}
	c, f := newUpgradeCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade", "o1", "", host.SceneC2C, "o1"))
	if got := lastReply(t, f); !strings.Contains(got, "检查更新失败") {
		t.Fatalf("应报告失败原因，实际: %q", got)
	}
}

// 验收 1 的核心：/upgrade apply 的顺序必须是
//
//	回消息 → 换盘计划 → 关停（由 main 观察）
//
// 用 fakeAPI 的发送前钩子断言"发消息那一刻计划还没入队"——顺序反了，
// 这条回复在重启后就永远发不出去。
func TestUpgradeApplyRepliesBeforeHandingPlan(t *testing.T) {
	plan := &upgrade.Plan{
		Exe: `C:\qqbot\qqbot.exe`, NewPath: `C:\qqbot\qqbot.exe.new`,
		From: "1.2.3", To: "1.2.4", Tag: "v1.2.4",
		StateDir: `C:\data\upgrade`, Snapshot: `C:\data\upgrade\snapshots\s.db`,
		Staged: 40 << 20,
		Plugins: []upgrade.PluginDelta{
			{Name: "echo", From: "0.1.0", To: "0.2.0", Action: "升"},
			{Name: "md", From: "0.2.0", To: "0.2.0", Action: "跳过（已是最新）"},
		},
	}
	svc := &fakeUpgrade{plan: plan}
	c, f := newUpgradeCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	queuedAtSend := -1
	f.onSend = func() { queuedAtSend = len(c.restart) }

	c.handleReserved(context.Background(), adminEvent("/upgrade apply", "o1", "", host.SceneC2C, "o1"))

	if queuedAtSend != 0 {
		t.Fatalf("回消息时计划已经入队了（队列 %d 条）：顺序反了，这条回复会在重启后丢失", queuedAtSend)
	}
	got := lastReply(t, f)
	for _, want := range []string{"升级已就位", "1.2.3 → 1.2.4", "已校验", "echo 0.1.0→0.2.0", "Resume"} {
		if !strings.Contains(got, want) {
			t.Errorf("回复缺少 %q:\n%s", want, got)
		}
	}
	// 跳过的插件不进这条消息（预览里已经说过一次）。
	if strings.Contains(got, "md") {
		t.Errorf("跳过的插件不该出现在 apply 回复里:\n%s", got)
	}

	select {
	case req := <-c.restart:
		if req.Plan == nil || req.Plan.To != "1.2.4" {
			t.Fatalf("换盘计划没有交给 main: %+v", req)
		}
		if req.By != "o1" || req.Role != store.RoleOwner {
			t.Errorf("重启请求的身份不对: %+v", req)
		}
		if !strings.Contains(req.Reason, "1.2.4") {
			t.Errorf("备注应写清目标版本: %q", req.Reason)
		}
	default:
		t.Fatal("应产生一条带换盘计划的重启请求")
	}
}

// 升级失败必须明确说"没做任何改动，服务继续运行"，并且不重启：
// 验收 4 要的正是这个——失败留在进程内，旧二进制与标记都不动。
func TestUpgradeApplyFailureKeepsServing(t *testing.T) {
	svc := &fakeUpgrade{applyErr: errors.New("校验和不匹配，已删除下载的产物")}
	c, f := newUpgradeCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade apply", "o1", "", host.SceneC2C, "o1"))

	got := lastReply(t, f)
	for _, want := range []string{"升级失败", "未做任何改动", "校验和不匹配"} {
		if !strings.Contains(got, want) {
			t.Errorf("回复缺少 %q:\n%s", want, got)
		}
	}
	if n := len(c.restart); n != 0 {
		t.Fatalf("失败时不该重启，实际 %d", n)
	}
}

// 已经是最新不是故障：不要回"失败"，也不重启。
func TestUpgradeApplyUpToDate(t *testing.T) {
	svc := &fakeUpgrade{applyErr: upgrade.ErrUpToDate}
	c, f := newUpgradeCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade apply", "o1", "", host.SceneC2C, "o1"))
	if got := lastReply(t, f); !strings.Contains(got, "已是最新版本") {
		t.Fatalf("应提示最新，实际: %q", got)
	}
	if n := len(c.restart); n != 0 {
		t.Fatalf("不该重启，实际 %d", n)
	}
}

// 验收 5（命令面）：enabled=false 时连 Preview/Apply 都不调用。
func TestUpgradeDisabledSkipsServiceEntirely(t *testing.T) {
	svc := &fakeUpgrade{}
	c, f := newUpgradeCore(t, svc)
	c.cfg = &config.Config{Upgrade: config.Upgrade{Enabled: boolPtr(false)}}
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	for _, content := range []string{"/upgrade", "/upgrade apply"} {
		c.handleReserved(context.Background(), adminEvent(content, "o1", "", host.SceneC2C, "o1"))
		if got := lastReply(t, f); !strings.Contains(got, "enabled = false") {
			t.Errorf("%s 应提示已关闭，实际: %q", content, got)
		}
	}
	if svc.previewCalls != 0 || svc.applyCalls != 0 {
		t.Error("关掉自更新后不该调用任何升级能力（那会发请求）")
	}
	// status 只读本地状态，仍然可用。
	c.handleReserved(context.Background(), adminEvent("/upgrade status", "o1", "", host.SceneC2C, "o1"))
	if svc.statusCalls != 1 {
		t.Errorf("status 应仍然可用，实际调用 %d 次", svc.statusCalls)
	}
}

func TestUpgradeStatusReportsPending(t *testing.T) {
	svc := &fakeUpgrade{status: upgrade.Status{
		Current: "1.2.4", Platform: "windows/amd64", Pending: true,
		State:     &upgrade.State{From: "1.2.3", To: "1.2.4", Attempts: 1},
		LastError: "",
	}}
	c, f := newUpgradeCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade status", "o1", "", host.SceneC2C, "o1"))
	got := lastReply(t, f)
	for _, want := range []string{"1.2.3 → 1.2.4", "第 1 次尝试"} {
		if !strings.Contains(got, want) {
			t.Errorf("状态正文缺少 %q:\n%s", want, got)
		}
	}
}

// /upgrade plugin 是 #45 的地盘：本票必须拒绝得干净（未知子命令 + 用法），
// 而不是假装成功。
func TestUpgradePluginNotImplemented(t *testing.T) {
	svc := &fakeUpgrade{}
	c, f := newUpgradeCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade plugin all", "o1", "", host.SceneC2C, "o1"))
	got := lastReply(t, f)
	if !strings.Contains(got, "尚未实现") || !strings.Contains(got, "/upgrade apply") {
		t.Fatalf("应说明未实现并给出用法:\n%s", got)
	}
	if svc.applyCalls != 0 || svc.previewCalls != 0 {
		t.Error("/upgrade plugin 不该触发核心升级")
	}
}

func TestUpgradeUnknownSubcommand(t *testing.T) {
	c, f := newUpgradeCore(t, &fakeUpgrade{})
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade nope", "o1", "", host.SceneC2C, "o1"))
	if got := lastReply(t, f); !strings.Contains(got, "未知子命令") {
		t.Fatalf("应提示未知子命令，实际: %q", got)
	}
}

func TestUpgradeHelp(t *testing.T) {
	c, f := newUpgradeCore(t, &fakeUpgrade{})
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade help", "o1", "", host.SceneC2C, "o1"))
	got := lastReply(t, f)
	for _, want := range []string{"/upgrade apply", "/upgrade status", "仅 Bot 所有者"} {
		if !strings.Contains(got, want) {
			t.Errorf("用法缺少 %q:\n%s", want, got)
		}
	}
}

// 没装配升级能力时命令仍要被核心消费：落到插件就等于把名字让了出去。
func TestUpgradeWithoutServiceIsConsumed(t *testing.T) {
	c, f := newUpgradeCore(t, nil)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	if !c.handleReserved(context.Background(), adminEvent("/upgrade", "o1", "", host.SceneC2C, "o1")) {
		t.Fatal("/upgrade 应被核心消费")
	}
	if got := lastReply(t, f); !strings.Contains(got, "未装配") {
		t.Fatalf("应提示未装配，实际: %q", got)
	}
}

// 插件不能声明 /upgrade：否则一个第三方插件就能让机器人把二进制换成任意东西。
func TestPluginCannotClaimUpgradePrefix(t *testing.T) {
	for _, prefix := range []string{"/upgrade", "/UPGRADE", "/upgrade apply"} {
		if _, err := NewRouter([]host.Plugin{&fakePlugin{m: manifest("evil", 1, prefix)}}); err == nil {
			t.Errorf("插件声明 %q 应被拒绝加载", prefix)
		}
	}
}

func boolPtr(v bool) *bool { return &v }

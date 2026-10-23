package core

import (
	"context"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/internal/upgrade"
	"github.com/knight000/go_qqbot/pkg/host"
)

// hotPlugin 是"进程隔离 + 可热替换"的插件替身：core 只靠类型断言认它
// （方法名对上即可，core 与 pluginproc 不互相 import）。
type hotPlugin struct {
	m   host.Manifest
	bin string
	// replaced 记录被要求换上的落位文件（core 不该自己动文件）。
	replaced []string
}

func (p *hotPlugin) Manifest() host.Manifest               { return p.m }
func (p *hotPlugin) Init(context.Context, host.Host) error { return nil }
func (p *hotPlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}
func (p *hotPlugin) BinPath() string { return p.bin }
func (p *hotPlugin) ReplaceBinary(newPath string) error {
	p.replaced = append(p.replaced, newPath)
	return nil
}

// pluginCore 装一个"有一个 echo 0.1.0 可热替换 + 一个 plain 不可热替换"的核心。
func pluginCore(t *testing.T, svc UpgradeService) (*Core, *fakeAPI, *hotPlugin) {
	t.Helper()
	echo := &hotPlugin{
		m:   host.Manifest{Name: "echo", Version: "0.1.0", APIVersion: host.APIVersion},
		bin: "/data/plugins/bin/echo",
	}
	plain := &fakePlugin{m: host.Manifest{Name: "plain", Version: "0.1.0", APIVersion: host.APIVersion}}
	c, f := newUpgradeCore(t, svc)
	c.plugins = []host.Plugin{echo, plain}
	return c, f, echo
}

// /upgrade plugin all：把每个已加载且可热替换的插件都交上去，名字与版本
// 取自 manifest，然后原样回报告正文。
func TestUpgradePluginAllTargetsLoadedPlugins(t *testing.T) {
	svc := &fakeUpgrade{pluginReport: upgrade.PluginReport{
		Tag: "v1.2.4", Platform: "windows-amd64",
		Results: []upgrade.PluginDelta{{Name: "echo", From: "0.1.0", To: "0.2.0", Action: "升", Size: 1 << 20}},
	}}
	c, f, _ := pluginCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	if !c.handleReserved(context.Background(), adminEvent("/upgrade plugin all", "o1", "", host.SceneC2C, "o1")) {
		t.Fatal("/upgrade 应被核心消费")
	}
	if svc.pluginCalls != 1 {
		t.Fatalf("应调用一次插件升级，实际 %d", svc.pluginCalls)
	}
	if len(svc.pluginTargets) != 1 || svc.pluginTargets[0].Name != "echo" {
		t.Fatalf("只该把可热替换的插件交上去: %+v", svc.pluginTargets)
	}
	if svc.pluginTargets[0].Version != "0.1.0" {
		t.Errorf("应带上已加载的版本，实际 %q", svc.pluginTargets[0].Version)
	}
	got := lastReply(t, f)
	for _, want := range []string{"插件升级", "0.1.0 → 0.2.0", "升", "耗时"} {
		if !strings.Contains(got, want) {
			t.Errorf("回复缺少 %q:\n%s", want, got)
		}
	}
	// 不能热替换的插件要被说出来，而不是悄悄漏掉。
	if !strings.Contains(got, "plain") {
		t.Errorf("应说明 plain 不支持热替换:\n%s", got)
	}
	if len(c.restart) != 0 {
		t.Error("插件热升级不该重启核心")
	}
}

// 单个插件：只交一个目标。
func TestUpgradePluginSingleName(t *testing.T) {
	svc := &fakeUpgrade{pluginReport: upgrade.PluginReport{Tag: "v1.2.4", Results: []upgrade.PluginDelta{
		{Name: "echo", From: "0.1.0", To: "0.2.0", Action: "升"},
	}}}
	c, f, echo := pluginCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade plugin echo", "o1", "", host.SceneC2C, "o1"))
	if len(svc.pluginTargets) != 1 || svc.pluginTargets[0].Name != "echo" {
		t.Fatalf("目标不对: %+v", svc.pluginTargets)
	}
	if strings.Contains(lastReply(t, f), "不支持热替换") {
		t.Error("单个插件时不该带 all 的附注")
	}
	if len(echo.replaced) != 0 {
		t.Error("core 不该自己换文件（那是插件代理的事）")
	}
}

// 名字不认识：回"没有这个插件"并列出可用的，且一个请求都不发。
func TestUpgradePluginUnknownNameListsAvailable(t *testing.T) {
	svc := &fakeUpgrade{}
	c, f, _ := pluginCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade plugin nope", "o1", "", host.SceneC2C, "o1"))
	got := lastReply(t, f)
	for _, want := range []string{"没有这个插件", "nope", "echo", "plain"} {
		if !strings.Contains(got, want) {
			t.Errorf("回复缺少 %q:\n%s", want, got)
		}
	}
	if svc.pluginCalls != 0 {
		t.Error("名字不对时不该发任何请求")
	}
}

// 已加载但不是独立进程的插件：说明它不能热替换，而不是谎称"没有这个插件"。
func TestUpgradePluginNotHotSwappable(t *testing.T) {
	svc := &fakeUpgrade{}
	c, f, _ := pluginCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade plugin plain", "o1", "", host.SceneC2C, "o1"))
	got := lastReply(t, f)
	if !strings.Contains(got, "plain") || !strings.Contains(got, "热替换") {
		t.Fatalf("应说明该插件不支持热替换:\n%s", got)
	}
	if svc.pluginCalls != 0 {
		t.Error("不可热替换时不该发请求")
	}
}

// 参数缺失：只回用法，不发请求。
func TestUpgradePluginWithoutNameShowsUsage(t *testing.T) {
	svc := &fakeUpgrade{}
	c, f, _ := pluginCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade plugin", "o1", "", host.SceneC2C, "o1"))
	if got := lastReply(t, f); !strings.Contains(got, "/upgrade plugin") {
		t.Fatalf("应给出用法:\n%s", got)
	}
	if svc.pluginCalls != 0 {
		t.Error("缺参数时不该发请求")
	}
}

// 关掉自更新：与核心升级同一口径，一个请求都不发。
func TestUpgradePluginDisabledSkipsService(t *testing.T) {
	svc := &fakeUpgrade{}
	c, f, _ := pluginCore(t, svc)
	c.cfg = &config.Config{Upgrade: config.Upgrade{Enabled: boolPtr(false)}}
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade plugin all", "o1", "", host.SceneC2C, "o1"))
	if got := lastReply(t, f); !strings.Contains(got, "enabled = false") {
		t.Fatalf("应提示自更新已关闭:\n%s", got)
	}
	if svc.pluginCalls != 0 {
		t.Error("关掉自更新后不该调用升级能力（那会发请求）")
	}
}

// 服务侧的失败（dev 版本、取不到索引）要能原样读到原因。
func TestUpgradePluginReportsServiceError(t *testing.T) {
	svc := &fakeUpgrade{pluginErr: upgrade.ErrDisabled}
	c, f, _ := pluginCore(t, svc)
	seedAdmin(t, c, "o1", "", store.RoleOwner)

	c.handleReserved(context.Background(), adminEvent("/upgrade plugin echo", "o1", "", host.SceneC2C, "o1"))
	got := lastReply(t, f)
	if !strings.Contains(got, "插件升级失败") || !strings.Contains(got, "enabled = false") {
		t.Fatalf("应报告失败原因:\n%s", got)
	}
}

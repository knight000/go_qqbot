package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/internal/upgrade"
	"github.com/knight000/go_qqbot/pkg/host"
)

// pendingFixture 造一份"已经换过盘、但新版本还没证明自己能活"的现场：
// exe 是新版本、.old 是旧版本、state.json 记着这次升级。
func pendingFixture(t *testing.T) (dir, exe string) {
	t.Helper()
	dataDir := t.TempDir()
	binDir := t.TempDir()
	exe = filepath.Join(binDir, "qqbot.exe")
	if err := os.WriteFile(exe, []byte("NEW-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upgrade.OldPath(exe), []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir = upgrade.StateDir(dataDir)
	if err := upgrade.SaveState(dir, &upgrade.State{
		Tag: "v1.2.4", From: "1.2.3", To: "1.2.4", Exe: exe, Attempts: 0,
	}); err != nil {
		t.Fatal(err)
	}
	return dir, exe
}

func readStr(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

// 验收 3：模拟"新版本启动即退出"——第一次启动只是记账，第二次启动
// 自动回滚到 .old 并要求重启。
func TestPendingUpgradeRollsBackOnSecondStart(t *testing.T) {
	dir, exe := pendingFixture(t)
	log := slog.New(slog.DiscardHandler)

	rolledBack, err := handlePendingUpgrade(dir, exe, log)
	if err != nil {
		t.Fatalf("第一次启动: %v", err)
	}
	if rolledBack {
		t.Fatal("第一次启动不该回滚：新版本总要有机会跑起来")
	}
	if got := readStr(t, exe); got != "NEW-BINARY" {
		t.Fatalf("第一次启动不该改二进制，实际 %q", got)
	}

	rolledBack, err = handlePendingUpgrade(dir, exe, log)
	if err != nil {
		t.Fatalf("第二次启动: %v", err)
	}
	if !rolledBack {
		t.Fatal("第二次启动应判定回滚并要求重启")
	}
	if got := readStr(t, exe); got != "OLD-BINARY" {
		t.Fatalf("回滚后 exe 应是旧二进制，实际 %q", got)
	}
	// 标记必须清掉，否则下一次启动会拿着已经不存在的 .old 再回滚一次。
	if st, err := upgrade.LoadState(dir); err != nil || st != nil {
		t.Fatalf("回滚后应清掉标记: %+v %v", st, err)
	}
}

// 没有标记的常规启动：什么都不做，也不该因为定位不到 exe 而报错。
func TestPendingUpgradeWithoutMarker(t *testing.T) {
	dir := upgrade.StateDir(t.TempDir())
	if rolledBack, err := handlePendingUpgrade(dir, "", slog.New(slog.DiscardHandler)); err != nil || rolledBack {
		t.Fatalf("没有标记时应静默返回: %v %v", rolledBack, err)
	}
}

// 需要回滚但定位不到 exe：不静默、也不清标记（下一次启动还有机会）。
func TestPendingUpgradeNeedsExeToRollback(t *testing.T) {
	dir, _ := pendingFixture(t)
	log := slog.New(slog.DiscardHandler)
	if _, err := handlePendingUpgrade(dir, "", log); err != nil {
		t.Fatalf("第一次启动: %v", err)
	}
	rolledBack, err := handlePendingUpgrade(dir, "", log)
	if err == nil || rolledBack {
		t.Fatalf("定位不到 exe 时应报错且不回滚: %v %v", rolledBack, err)
	}
	if st, err := upgrade.LoadState(dir); err != nil || st == nil {
		t.Fatalf("标记应保留: %+v %v", st, err)
	}
}

// 观察窗结束就删标记：那是"新版本算启动成功"的唯一凭据。
func TestClearPendingAfterRemovesMarker(t *testing.T) {
	dir, _ := pendingFixture(t)
	clearPendingAfter(context.Background(), dir, 10*time.Millisecond, slog.New(slog.DiscardHandler))
	if st, err := upgrade.LoadState(dir); err != nil || st != nil {
		t.Fatalf("观察窗结束后应删掉标记: %+v %v", st, err)
	}
}

// 观察窗内收到退出信号 → 不宣布成功，标记留着给下次启动判断。
func TestClearPendingAfterHonoursShutdown(t *testing.T) {
	dir, _ := pendingFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clearPendingAfter(ctx, dir, 10*time.Millisecond, slog.New(slog.DiscardHandler))
	if st, err := upgrade.LoadState(dir); err != nil || st == nil {
		t.Fatalf("退出信号之后应保留标记: %+v %v", st, err)
	}
}

// 没有标记的常规启动不该为一个不存在的升级等 30 秒。
func TestClearPendingAfterWithoutMarkerReturnsImmediately(t *testing.T) {
	dir := upgrade.StateDir(t.TempDir())
	start := time.Now()
	clearPendingAfter(context.Background(), dir, 5*time.Second, slog.New(slog.DiscardHandler))
	if d := time.Since(start); d > time.Second {
		t.Fatalf("没有标记时不该等待，实际等了 %s", d)
	}
}

// 预览里的插件清单只认已加载插件的 manifest。
func TestLoadedPlugins(t *testing.T) {
	got := loadedPlugins([]host.Plugin{
		fakePlugin{name: "echo"}, fakePlugin{name: "md"},
	})
	if len(got) != 2 || got[0].Name != "echo" || got[1].Name != "md" {
		t.Fatalf("插件清单不对: %+v", got)
	}
	if loadedPlugins(nil) == nil {
		t.Error("空清单应返回空切片而不是 nil（调用方要 len 它）")
	}
}

// binPlugin 是能报出自己可执行文件路径的插件（生产上是 pluginproc.proxy）。
type binPlugin struct {
	fakePlugin
	bin string
}

func (p binPlugin) BinPath() string { return p.bin }

// 插件是可执行文件：核心升级要把更高版本的产物落位到它旁边（同卷），
// 所以路径必须由装配期交上来；不是子进程的插件留空，升级侧据此跳过。
func TestLoadedPluginsCarriesBinPath(t *testing.T) {
	got := loadedPlugins([]host.Plugin{binPlugin{fakePlugin: fakePlugin{name: "echo"}, bin: "/data/plugins/bin/echo"}})
	if len(got) != 1 || got[0].Bin != "/data/plugins/bin/echo" {
		t.Fatalf("应带上插件路径: %+v", got)
	}
	plain := loadedPlugins([]host.Plugin{fakePlugin{name: "md"}})
	if plain[0].Bin != "" {
		t.Errorf("没有进程的插件不该编出一个路径: %q", plain[0].Bin)
	}
}

// 换盘计划只在 /upgrade apply 时出现：常规 /reload 的请求不该带计划，
// 否则 main 会去换一份不存在的 .new。
func TestReloadRequestCarriesNoPlan(t *testing.T) {
	var req core.RestartRequest
	if req.Plan != nil {
		t.Fatal("零值重启请求不该带换盘计划")
	}
}

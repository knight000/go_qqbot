package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/knight000/go_qqbot/internal/buildinfo"
	"github.com/knight000/go_qqbot/internal/upgrade"
	"github.com/knight000/go_qqbot/pkg/host"
)

// upgradeHealthyWindow 是"新版本算启动成功"的观察窗（ADR-0006 决策 6）。
//
// 用"网关协程存活 30 秒"而不是"网关连上了"：网络抖动不该触发回滚，
// 而一个连不上网关的版本通常也是网络问题，不是版本问题。
const upgradeHealthyWindow = 30 * time.Second

// handlePendingUpgrade 在启动早期（配置已加载、拿单实例锁之前）处理上次
// 升级留下的标记，返回 true 表示"已回滚，需要立刻重启自身"。
//
// 为什么必须在拿锁之前：回滚的最后一步是重新执行自身，而旧进程此刻还没
// 持有任何锁——先拿锁再重启，新进程会等一个注定不会释放的锁。
//
// 这不是恢复"exec 失败"（那条已经在 Stage 里判掉了），而是兜住
// "新版本起来了但活不过观察窗"这一种形态：进程没法在崩溃时自救，
// 只能靠下一次启动读标记。
func handlePendingUpgrade(dir, exe string, log *slog.Logger) (bool, error) {
	d, err := upgrade.CheckPending(dir)
	if err != nil {
		// 标记坏了就跳过判断（不猜），文件原样留着给人看。
		log.Warn("读取升级标记失败，跳过回滚判断", "dir", dir, "err", err)
		return false, nil
	}
	if !d.Pending {
		return false, nil
	}
	log.Warn("检测到未确认的升级",
		"from", d.State.From, "to", d.State.To, "attempts", d.State.Attempts,
		"rollback", d.Rollback, "exe", exe)
	if !d.Rollback {
		return false, nil
	}

	if exe == "" {
		// 保留标记：下一次启动还有机会。
		return false, fmt.Errorf("检测到需要回滚（%s），但定位不到可执行文件", d.Reason)
	}
	if err := upgrade.Restore(exe); err != nil {
		// 回滚不了就别留着标记：它会让每一次启动都重复走这条路。
		_ = upgrade.ClearState(dir)
		return false, fmt.Errorf("回滚到旧版本失败: %w", err)
	}
	// 标记必须先清掉再重启：留着它，新起的旧版本会立刻再判一次回滚，
	// 而那时已经没有 .old 了。
	_ = upgrade.ClearState(dir)
	log.Error("新版本启动后未能稳定运行，已回滚到旧版本并重新启动",
		"reason", d.Reason, "exe", exe)
	return true, nil
}

// clearPendingAfter 在观察窗结束后删掉 pending 标记。
//
// 调用点必须在网关协程已经起来之后（core.Start 完成 + 网关存活），
// 而且要挂在这个进程的 ctx 上：30 秒内收到退出信号就不该宣布"成功"。
//
// 已知边角（ADR-0006 接受的）：30 秒内有人敲 /reload，新进程会把
// attempts 再 +1，下一次启动会误判成回滚。代价是退回上一版并清掉标记。
func clearPendingAfter(ctx context.Context, dir string, wait time.Duration, log *slog.Logger) {
	// 没有标记的常规启动不该起一个 30 秒的定时器。
	st, err := upgrade.LoadState(dir)
	if err != nil || st == nil {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		log.Warn("观察窗内收到退出信号，保留升级标记待下次启动判定")
		return
	case <-t.C:
	}
	if err := upgrade.MarkHealthy(dir); err != nil {
		log.Warn("清除升级标记失败", "dir", dir, "err", err)
		return
	}
	log.Info("新版本已稳定运行，升级标记已清除",
		"version", buildinfo.Get().String(), "window", wait.String())
}

// loadedPlugins 把已装配插件的名字与版本交给升级包，用于预览里的
// "插件升/跳过清单"。
//
// 只有已加载的插件在册：被 [plugins].disabled 停用或加载失败的插件，
// 核心根本不知道它们的版本号。这条限制会写进预览正文（"只列已加载的插件"），
// 否则"某插件没出现在清单里"会被读成"它不需要升级"。
func loadedPlugins(plugins []host.Plugin) []upgrade.LoadedPlugin {
	out := make([]upgrade.LoadedPlugin, 0, len(plugins))
	for _, p := range plugins {
		m := p.Manifest()
		out = append(out, upgrade.LoadedPlugin{Name: m.Name, Version: m.Version})
	}
	return out
}

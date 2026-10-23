package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/upgrade"
	"github.com/knight000/go_qqbot/pkg/host"
)

// /upgrade 是自更新的唯一入口（ADR-0006 决策 1/2）：
// 只在人主动敲的时候发请求，启动期不查。
//
// 四段命令里本票只实现三段（预览 / 应用核心 / 状态）；`/upgrade plugin`
// 留给 #45，落到「未知子命令 + 用法」，而不是假装成功。
//
// 权限是 owner-only 而**不是** admin：升级通道就是远程代码执行通道
// （ADR-0006 的"代价"第一条），辅助管理员不该有这个能力。
const upgradeUsage = `用法：
/upgrade               预览：当前版本、最新版本、核心产物大小、插件升/跳过清单（不改动任何文件）
/upgrade apply         应用核心升级：快照数据库 → 下载 → 校验 → 落位 → 重启进程
/upgrade status        查看是否还有未确认的升级、上次失败原因
/upgrade plugin [...]  插件升级（尚未实现）

仅 Bot 所有者可用。版本号为 dev 时拒绝（dev 无法参与版本比较）。
安全提示：/upgrade apply 会把本机的 qqbot 换成远端发布的版本，请确认源可信。`

// UpgradeService 是 /upgrade 依赖的全部能力。
//
// 用接口注入而不是让 core 直接持有 *upgrade.Upgrader：core 的单测从此
// 既不发网络、也不碰真二进制，而命令面的行为（权限、子命令、回复顺序、
// 换盘计划的交接）全部可测。
type UpgradeService interface {
	Preview(ctx context.Context) (upgrade.Report, error)
	Apply(ctx context.Context) (*upgrade.Plan, error)
	Status(ctx context.Context) (upgrade.Status, error)
}

func (c *Core) handleUpgrade(ctx context.Context, ev *host.Event) {
	p := c.authorize(ctx, ev)
	if p.Role != host.RoleOwner {
		// 与 /reload 一致：核心指令明确回一句，而不是静默，
		// 也不泄露可用版本、源地址之类任何信息。
		c.replyText(ctx, ev, "该指令仅所有者可用。")
		return
	}

	fields := strings.Fields(strings.TrimSpace(ev.Content))
	var sub string
	if len(fields) > 1 {
		sub = strings.ToLower(fields[1])
	}

	if c.upgrade == nil {
		c.replyText(ctx, ev, "自更新未装配：本进程没有可用的升级能力，只能手工替换二进制。")
		return
	}

	switch sub {
	case "", "check", "preview":
		if !c.upgradeAllowed(ctx, ev) {
			return
		}
		c.upgradePreview(ctx, ev)
	case "apply":
		if !c.upgradeAllowed(ctx, ev) {
			return
		}
		c.upgradeApply(ctx, ev)
	case "status":
		// status 只读本地状态，不发请求：关掉自更新之后仍然可以看
		// "有没有上次没确认的升级"。
		c.upgradeStatus(ctx, ev)
	case "plugin", "plugins":
		c.replyText(ctx, ev, "插件升级尚未实现（见 #45），本版本只支持核心升级。\n\n"+upgradeUsage)
	case "help", "-h", "--help":
		c.replyText(ctx, ev, upgradeUsage)
	default:
		c.replyText(ctx, ev, "未知子命令 "+fields[1]+"。\n\n"+upgradeUsage)
	}
}

// upgradeAllowed 在真正调用之前短路 [upgrade].enabled=false。
//
// 服务自己也会拒绝（连请求都不发），这里再拦一次是为了让回复指向配置项：
// 用户看到的必须是"你把它关了"，而不是一条来自深处的 ErrDisabled。
func (c *Core) upgradeAllowed(ctx context.Context, ev *host.Event) bool {
	if c.cfg != nil && !c.cfg.UpgradeEnabled() {
		c.replyText(ctx, ev, "自更新已在配置里关闭（[upgrade].enabled = false），不会发出任何请求。")
		return false
	}
	return true
}

func (c *Core) upgradePreview(ctx context.Context, ev *host.Event) {
	rep, err := c.upgrade.Preview(ctx)
	if err != nil {
		c.log.Warn("检查更新失败", "err", err)
		c.replyText(ctx, ev, "检查更新失败："+err.Error())
		return
	}
	c.replyText(ctx, ev, rep.Text())
}

// upgradeApply 的顺序是本票最关键的一处约定（决策 8）：
//
//	快照 → 下载 → 校验 → 落位（由 UpgradeService.Apply 在进程内完成）
//	→ 回消息 → 把换盘计划交给 main
//
// 为什么先回消息再入队：被动回复窗口只在收到消息时存在，进程重启后这条
// 回复就再也发不出去了。/reload 踩过这个坑，这里照抄。
//
// 为什么换盘不在进程内做：真正换 exe 必须发生在网关断开、session 落盘、
// 数据库关闭之后，否则新进程会和旧进程抢同一个 gateway session 与 SQLite
// 连接。所以这里只交出计划，动手的是 main（run 返回之后）。
func (c *Core) upgradeApply(ctx context.Context, ev *host.Event) {
	plan, err := c.upgrade.Apply(ctx)
	if err != nil {
		switch {
		case errors.Is(err, upgrade.ErrUpToDate):
			c.replyText(ctx, ev, "当前已是最新版本，无需升级。")
		case errors.Is(err, upgrade.ErrDisabled):
			c.replyText(ctx, ev, "自更新已在配置里关闭（[upgrade].enabled = false）。")
		default:
			c.log.Error("准备升级失败", "err", err)
			c.replyText(ctx, ev, "升级失败（未做任何改动，服务继续运行）："+err.Error())
		}
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "升级已就位：%s → %s\n", plan.From, plan.To)
	if plan.Snapshot != "" {
		fmt.Fprintf(&b, "数据库快照 %s\n", plan.Snapshot)
	}
	fmt.Fprintf(&b, "产物已校验并落位到 %s\n", plan.NewPath)
	if ups := planUpgrades(plan.Plugins); len(ups) > 0 {
		fmt.Fprintf(&b, "插件（只列已加载的插件）%s\n", strings.Join(ups, "、"))
	}
	b.WriteString("接下来重启进程换盘；重启后走 Resume，不额外消耗 Identify 配额。")
	b.WriteString("用 /ping 对比版本与启动时间即可确认生效。")

	// 先回消息，再入队：顺序反了这条回复必丢。
	c.replyText(ctx, ev, b.String())

	req := RestartRequest{
		By:     ev.SenderID,
		Role:   string(ev.Actor.Role),
		Scene:  ev.Scene,
		Reason: "upgrade " + plan.From + " -> " + plan.To,
		At:     time.Now(),
		Plan:   plan,
	}
	if err := c.requestRestart(req); err != nil {
		// 已经回了一条"已就位"的消息，这里必须补一条：否则 owner 会以为
		// 升级会自己发生，而实际上没有任何人会去换盘。
		c.log.Warn("升级计划未能入队", "err", err, "to", plan.To)
		c.replyText(ctx, ev, "但已有重启请求在处理，本次不会换盘。"+
			"等重启结束后重新执行 /upgrade apply（会先清掉上次落位的文件）。")
		return
	}
	c.log.Warn("收到 /upgrade apply，准备换盘并重启",
		"by", ev.SenderID, "from", plan.From, "to", plan.To,
		"exe", plan.Exe, "bytes", plan.Staged, "snapshot", plan.Snapshot)
}

func (c *Core) upgradeStatus(ctx context.Context, ev *host.Event) {
	st, err := c.upgrade.Status(ctx)
	if err != nil {
		c.log.Warn("读取升级状态失败", "err", err)
		c.replyText(ctx, ev, "读取升级状态失败："+err.Error())
		return
	}
	c.replyText(ctx, ev, st.Text())
}

// planUpgrades 把计划里的插件清单收成一行：只报真正会升的，
// 跳过的留给预览——跳过的原因已经在预览里说过一次了。
func planUpgrades(plugins []upgrade.PluginDelta) []string {
	out := make([]string, 0, len(plugins))
	for _, p := range plugins {
		if p.Action == "升" {
			out = append(out, p.Name+" "+p.From+"→"+p.To)
		}
	}
	return out
}

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
// 四段命令：预览 / 应用核心 / 应用插件 / 状态。插件那条只重启那个插件的
// 子进程（决策 6：回滚几乎免费），所以它不需要重启核心、也不需要二次确认。
//
// 权限是 owner-only 而**不是** admin：升级通道就是远程代码执行通道
// （ADR-0006 的"代价"第一条），辅助管理员不该有这个能力。
const upgradeUsage = `用法：
/upgrade                    预览：当前版本、最新版本、核心产物大小、插件升/跳过清单（不改动任何文件）
/upgrade apply              应用核心升级：快照数据库 → 下载 → 校验 → 落位 → 重启进程
/upgrade plugin <名称|all>  升级单个插件（或全部已加载插件）：只重启那个插件的子进程
/upgrade status             查看是否还有未确认的升级、上次失败原因

仅 Bot 所有者可用。版本号为 dev 时拒绝（dev 无法参与版本比较）。
安全提示：/upgrade apply 会把本机的 qqbot 换成远端发布的版本，请确认源可信。`

// PluginReplacer 是插件进程代理暴露的两个可选能力（pluginproc.proxy 实现）。
//
// 只在方法名上对齐，两个包不必互相 import：pluginproc 已经 import core
// （装配期校验复用路由构建器），core 不能再反向依赖它。方法集与
// upgrade.PluginReplacer 逐字相同，所以断言拿到的值可以直接交给升级包。
type PluginReplacer interface {
	// BinPath 是插件可执行文件的绝对路径（落位文件必须与它同目录）。
	BinPath() string
	// ReplaceBinary 换掉这个插件的产物并只重启它；失败时自己回滚。
	ReplaceBinary(newPath string) error
}

// UpgradeService 是 /upgrade 依赖的全部能力。
//
// 用接口注入而不是让 core 直接持有 *upgrade.Upgrader：core 的单测从此
// 既不发网络、也不碰真二进制，而命令面的行为（权限、子命令、回复顺序、
// 换盘计划的交接）全部可测。
type UpgradeService interface {
	Preview(ctx context.Context) (upgrade.Report, error)
	Apply(ctx context.Context) (*upgrade.Plan, error)
	// UpgradePlugins 升级给定的插件，逐条给出结论。返回的 error 只表示
	// "整条路走不通"（关掉自更新、dev 版本、取不到索引）。
	UpgradePlugins(ctx context.Context, targets []upgrade.PluginTarget) (upgrade.PluginReport, error)
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
		if !c.upgradeAllowed(ctx, ev) {
			return
		}
		var rest []string
		if len(fields) > 2 {
			rest = fields[2:]
		}
		c.upgradePlugin(ctx, ev, rest)
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
	if fails := planPluginFailures(plan.Plugins); len(fails) > 0 {
		fmt.Fprintf(&b, "插件未升级：%s\n", strings.Join(fails, "、"))
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

// upgradePlugin 是 `/upgrade plugin <名称|all>`（决策 2）。
//
// 参数里的名字必须是**已加载**的插件：核心才知道它们的版本号，索引里多出来
// 的插件在本机没有进程可换。这条限制也顺手挡住"敲错名字却以为升了"。
//
// 与核心升级不同，这条路不重启核心：换掉一个插件只需要重启它的子进程，
// 所以回复就是最终结果——没有"接下来重启"这句话。
func (c *Core) upgradePlugin(ctx context.Context, ev *host.Event, args []string) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		c.replyText(ctx, ev, "用法：/upgrade plugin <名称|all>\n\n"+upgradeUsage)
		return
	}

	targets, missing, cold := c.pluginTargets(args[0])
	if missing != "" {
		c.replyText(ctx, ev, "没有这个插件 "+missing+"。\n"+c.pluginListText())
		return
	}
	if len(targets) == 0 {
		// 走到这里的两种情形：本机没有已加载插件；或者点了名但那个插件不是
		// 独立进程（cold 非空）。后者必须说清"不能"，而不是"没有"。
		if len(cold) > 0 {
			c.replyText(ctx, ev, "插件 "+strings.Join(cold, "、")+
				" 不是独立进程（不支持热替换），只能重启核心让它重新装配。")
			return
		}
		c.replyText(ctx, ev, "当前没有已加载的插件，没有可升级的对象。")
		return
	}

	rep, err := c.upgrade.UpgradePlugins(ctx, targets)
	if err != nil {
		// 一个请求都没发、或整条路走不通（关掉自更新、dev 版本、取不到索引）。
		// 与核心升级同一口径：说清原因，服务继续跑。
		c.log.Warn("插件升级失败", "err", err)
		c.replyText(ctx, ev, "插件升级失败（未做任何改动）："+err.Error())
		return
	}

	text := rep.Text()
	if len(cold) > 0 {
		// all 的附注：漏掉它们等于谎报"全都处理过了"。
		text += "\n\n以下插件不是独立进程，不支持热替换（只能靠重启核心）：" +
			strings.Join(cold, "、")
	}
	c.replyText(ctx, ev, text)
	c.log.Warn("收到 /upgrade plugin",
		"by", ev.SenderID, "arg", args[0], "targets", len(targets),
		"changed", rep.Changed(), "elapsed", rep.Elapsed.String())
}

// pluginTargets 把命令参数翻成升级目标。
//
// 返回：
//   - targets：可热替换的目标（all 时是全部，否则只有一个）
//   - missing：这个名字既不在已加载插件里，也不是 all（调用方回"没有这个插件"）
//   - cold   ：名字对上了、但这个插件不是独立进程（没有 ReplaceBinary）
//
// 顺序由升级包按 index.json 排（all 的报告要能逐行比对），这里只负责挑选。
func (c *Core) pluginTargets(arg string) (targets []upgrade.PluginTarget, missing string, cold []string) {
	arg = strings.TrimSpace(arg)
	all := strings.EqualFold(arg, "all")
	found := false
	for _, p := range c.plugins {
		m := p.Manifest()
		if !all && m.Name != arg {
			continue
		}
		found = true
		r, ok := p.(PluginReplacer)
		if !ok {
			cold = append(cold, m.Name)
			continue
		}
		targets = append(targets, upgrade.PluginTarget{
			Name:     m.Name,
			Version:  m.Version,
			Upstream: m.Upstream,
			Replace:  r,
		})
	}
	if !all && !found {
		return nil, arg, nil
	}
	return targets, "", cold
}

// pluginListText 列出可用的插件名：敲错名字时唯一有用的信息就是这一行。
func (c *Core) pluginListText() string {
	names := make([]string, 0, len(c.plugins))
	for _, p := range c.plugins {
		names = append(names, p.Manifest().Name)
	}
	if len(names) == 0 {
		return "当前没有已加载的插件。"
	}
	return "可用的插件：" + strings.Join(names, "、") + "（all 表示全部）。"
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

// planPluginFailures 单独列"本该升却没升成"的插件。
//
// 纯跳过不在这里出现（预览里已经说过），而落位或校验失败是**新信息**：
// 不说的话 owner 会以为插件跟着核心一起升了，重启之后才发现版本没变。
func planPluginFailures(plugins []upgrade.PluginDelta) []string {
	out := make([]string, 0, len(plugins))
	for _, p := range plugins {
		if strings.HasPrefix(p.Action, "失败") {
			out = append(out, p.Name+" "+p.Action)
		}
	}
	return out
}

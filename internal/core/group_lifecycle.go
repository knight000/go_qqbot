package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// 群生命周期：机器人被拉进群、被移出群。
//
// 为什么值得单独处理：
//   - 它把"机器人被拉进群"与"所有者决定是否放行"之间的时序补齐了。
//     此前只能等新群里的第一条消息才被动登记，而那条消息往往正是
//     用户 @ 机器人问点什么——于是第一条消息必然被白名单丢掉。
//   - 它是 qq_groups 在群状态（left_at）的权威来源。平台没有"列出机器人
//     加入了哪些群"的接口，退群事件是唯一能知道"这个群已经不在了"的途径。
//
// 处理位置在群准入闸门之前（见 Core.Handle）：新群此刻必然未放行，
// 走闸门会被直接丢掉。这两个都是机器人自身的状态变化，不是群消息，
// 闸门对它们本就没有意义。

// handleGroupLifecycle 处理群生命周期通知，返回 true 表示事件已被消费。
func (c *Core) handleGroupLifecycle(ctx context.Context, ev *host.Event) bool {
	switch ev.Notice {
	case host.NoticeBotJoinedGroup:
		c.onBotJoinedGroup(ctx, ev)
		return true
	case host.NoticeBotLeftGroup:
		c.onBotLeftGroup(ctx, ev)
		return true
	}
	return false
}

// onBotJoinedGroup：入群立刻建档，取到名字后私聊通知所有者。
//
// 名字与通知都放到后台：取名字是一次最长 8s 的 HTTP 调用，
// 而 Handle 就是网关的读循环，压在这里等于让整条消息链路陪着等。
func (c *Core) onBotJoinedGroup(ctx context.Context, ev *host.Event) {
	groupID := ev.PeerID
	if groupID == "" {
		c.log.Warn("入群事件缺少 group_openid，忽略", "event", ev.ID)
		return
	}
	// 先落库：即便后面取名字失败、通知发不出去，群也已经登记好了。
	if err := c.store.MarkGroupPresent(ctx, groupID); err != nil {
		c.log.Error("登记入群失败", "group", groupID, "err", err)
		return
	}
	// per-peer 任务的群实例必须在这里补：重启时 Register 只遍历了当时的在群，
	// 之后新进来的群没有任何别的时机能被展开。
	c.expandPeerJobs(ctx, groupID)
	c.log.Info("机器人已加入群", "group", groupID, "操作人", ev.SenderID)
	c.trace.GroupLifecycle("加入群", groupID)

	operator := ev.SenderID
	fetch := c.cfg.GroupNameFetchEnabled()
	go func() {
		// 不能用 ev 的 ctx：事件处理返回后它可能已被取消。
		bg, cancel := context.WithTimeout(context.Background(), groupInfoTimeout+2*time.Second)
		defer cancel()

		name := ""
		if fetch {
			var err error
			name, err = c.lookupGroupName(bg, groupID)
			switch {
			case errors.Is(err, ErrGroupInfoForbidden):
				c.log.Info("群信息接口无访问权限（11253），新群名称留空；可用 /group name 设置",
					"group", groupID)
			case err != nil:
				c.log.Debug("新群取名称失败，留空", "group", groupID, "err", err)
			case name != "":
				if err := c.store.SetGroupName(bg, groupID, name, "api"); err != nil {
					c.log.Warn("写入群名称失败", "group", groupID, "err", err)
				}
			}
		}
		c.notifyGroupJoined(bg, groupID, operator, name)
	}()
}

func (c *Core) notifyGroupJoined(ctx context.Context, groupID, operator, name string) {
	g, ok, err := c.store.GetGroup(ctx, groupID)
	if err != nil {
		c.log.Warn("读取群信息失败，通知里省略准入状态", "group", groupID, "err", err)
	}

	var b strings.Builder
	b.WriteString("机器人加入了新群\n")
	fmt.Fprintf(&b, "群 %s\n", groupID)
	if name != "" {
		fmt.Fprintf(&b, "名称 %s（自动获取）\n", name)
	} else {
		b.WriteString("名称 未获取到（可用 /group name 设置）\n")
	}
	if operator != "" {
		fmt.Fprintf(&b, "操作人 %s\n", operator)
	}
	if ok {
		if allowed, reason := c.evaluateGroup(g); allowed {
			b.WriteString("状态 已放行\n")
		} else {
			fmt.Fprintf(&b, "状态 消息会被丢弃（%s）\n", reason)
		}
	}

	// 提醒的价值在于"下一步能做什么"，所以把两条指令直接写成可复制的形式。
	fmt.Fprintf(&b, "\n放行 /group allow %s", groupID)
	fmt.Fprintf(&b, "\n改名 /group name %s <名称>", groupID)
	c.notifyGroupEvent(ctx, b.String())
}

// onBotLeftGroup：标记已退出并通知。退群后不再取名字——
// 接口对已退出的群通常直接报错，而库里已有的名字足够认出是哪个群。
func (c *Core) onBotLeftGroup(ctx context.Context, ev *host.Event) {
	groupID := ev.PeerID
	if groupID == "" {
		c.log.Warn("退群事件缺少 group_openid，忽略", "event", ev.ID)
		return
	}
	if err := c.store.MarkGroupLeft(ctx, groupID); err != nil {
		c.log.Error("标记退群失败", "group", groupID, "err", err)
		return
	}
	// 按 R1 语义清掉该群的会话状态：群都不在了，插件在这里攒的对话进度
	// 没有任何意义，留着只会在机器人被重新拉回来时表现为"它还记着上次"。
	// 失败不阻断——退群标记才是关键，KV 可以留给 /reset session 清。
	if counts, err := c.store.ClearPeerSession(ctx, groupID); err != nil {
		c.log.Warn("清理退群会话失败", "group", groupID, "err", err)
	} else if counts.Total() > 0 {
		c.log.Info("已清理退群会话状态", "group", groupID,
			"kv", counts.KVRows, "sessions", counts.Sessions)
	}
	c.log.Info("机器人已退出群", "group", groupID, "操作人", ev.SenderID)
	c.trace.GroupLifecycle("退出群", groupID)

	operator := ev.SenderID
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		c.notifyGroupLeft(bg, groupID, operator)
	}()
}

func (c *Core) notifyGroupLeft(ctx context.Context, groupID, operator string) {
	g, ok, err := c.store.GetGroup(ctx, groupID)
	if err != nil {
		c.log.Warn("读取群信息失败，通知里省略名称", "group", groupID, "err", err)
	}

	var b strings.Builder
	b.WriteString("机器人已退出群\n")
	fmt.Fprintf(&b, "群 %s\n", groupID)
	if ok && g.Name != "" {
		fmt.Fprintf(&b, "名称 %s\n", g.Name)
	}
	if operator != "" {
		fmt.Fprintf(&b, "操作人 %s\n", operator)
	}
	b.WriteString("该群已标记为已退出：不再参与群名刷新，也不再计入在群数量。\n")
	if ok && g.Mode != store.GroupModeNone {
		// 名单刻意保留：重新把机器人拉回群时不需要再授权一次。
		fmt.Fprintf(&b, "名单仍保留（%s），重新入群后自动生效。", modeLabel(g.Mode))
	}
	c.notifyGroupEvent(ctx, b.String())
}

// notifyGroupEvent 把群生命周期通知私聊给配置的收件人。
//
// 走主动消息（没有 msg_id 可回），因此受 [active].enabled 与每 peer 日预算约束，
// 但**不走** [active].plugins 白名单：这是基础设施级通知，
// 不该要求用户把一个叫 "core" 的插件名写进配置才能收到。
func (c *Core) notifyGroupEvent(ctx context.Context, text string) {
	mode := c.notifyMode()
	if mode == config.NotifyOff {
		return
	}
	text = strings.TrimRight(text, "\n")
	targets, err := c.store.AdminTargets(ctx)
	if err != nil {
		c.log.Warn("读取通知收件人失败", "err", err)
		return
	}

	onlyOwner := mode == config.NotifyOwner
	sent, failed := 0, 0
	for _, t := range targets {
		if onlyOwner && t.Role != store.RoleOwner {
			continue
		}
		if t.C2COpenID == "" {
			continue
		}
		if err := c.sender.SendActive(ctx, host.SceneC2C, t.C2COpenID,
			host.Reply{Content: text}); err != nil {
			failed++
			c.log.Warn("群生命周期通知发送失败",
				"role", t.Role, "peer", t.C2COpenID, "err", err)
			continue
		}
		sent++
	}
	// 一条都没发出去必须留痕：否则表现为"机器人被拉进群了，但什么都没发生"，
	// 而真实原因可能只是收件人从没和机器人私聊过、或主动消息被关掉了。
	if sent == 0 {
		c.log.Warn("群生命周期通知未送达任何人",
			"收件人", len(targets), "失败", failed, "模式", mode,
			"提示", "检查 [active].enabled，以及收件人是否可用于单聊")
	}
}

// notifyMode 读 [group].notify；配置缺失只可能出现在测试里，按默认值处理。
func (c *Core) notifyMode() string {
	if c.cfg == nil {
		return config.NotifyOwner
	}
	return c.cfg.GroupNotifyMode()
}

// --- 批量刷新群名 ---

// refreshStats 是一次批量刷新的结果，也是汇报正文的唯一数据来源。
type refreshStats struct {
	Total     int
	Updated   int // 名字确实变了并已写库
	Same      int // 取到但与库里一致
	Empty     int // 接口返回空名
	Manual    int // 手工名，跳过（自动取名不得覆盖）
	Failed    int // 接口失败
	Forbidden bool
	Aborted   bool // 超时提前结束，未处理完
}

// groupRefresh 是 /group refresh 的实现入口。
//
// 立即回执 + 后台执行：Handle 是网关的读循环，同步跑 N 个最长 8s 的 HTTP
// 会把整条消息链路卡住。刷新不赶时间，放后台跑、跑完私聊汇报更合理。
func (c *Core) groupRefresh(ctx context.Context, ev *host.Event, args []string) {
	if len(args) > 0 {
		c.replyText(ctx, ev, "用法：/group refresh")
		return
	}
	groups, err := c.store.ListGroupsInUse(ctx)
	if err != nil {
		c.log.Error("读取在群列表失败", "err", err)
		c.replyText(ctx, ev, "读取群列表失败，请查看日志。")
		return
	}
	if len(groups) == 0 {
		c.replyText(ctx, ev, "没有在群的记录。已退出的群不参与刷新。")
		return
	}
	if !c.refreshing.CompareAndSwap(false, true) {
		c.replyText(ctx, ev, "已有刷新任务在执行，完成后再试。")
		return
	}

	c.replyText(ctx, ev, fmt.Sprintf(
		"开始刷新 %d 个群的名称，逐个调用群信息接口，完成后私聊汇报。", len(groups)))
	c.log.Info("开始刷新群名称", "count", len(groups), "by", ev.SenderID)

	requester := ev.SenderID
	go c.runGroupNameRefresh(requester, groups)
}

// runGroupNameRefresh 串行刷新。串行不是保守，是必须：
// 群信息接口限额 30 QPM，并发只会把配额更快打满并让整体更慢。
func (c *Core) runGroupNameRefresh(requester string, groups []store.Group) {
	defer c.refreshing.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), refreshBudget(len(groups)))
	defer cancel()

	st := refreshStats{Total: len(groups)}
	for _, g := range groups {
		if ctx.Err() != nil {
			st.Aborted = true
			break
		}
		// 手工名是管理员的明确意图，自动刷新不该碰它——即便接口能取到。
		if g.NameSource == "manual" {
			st.Manual++
			continue
		}
		name, err := c.lookupGroupName(ctx, g.OpenID)
		if err != nil {
			st.Failed++
			if errors.Is(err, ErrGroupInfoForbidden) {
				st.Forbidden = true
			}
			c.log.Debug("刷新群名称失败", "group", g.OpenID, "err", err)
			continue
		}
		switch {
		case name == "":
			st.Empty++
			continue
		case name == g.Name:
			st.Same++
			continue
		}
		if err := c.store.SetGroupName(ctx, g.OpenID, name, "api"); err != nil {
			st.Failed++
			c.log.Warn("写入群名称失败", "group", g.OpenID, "err", err)
			continue
		}
		st.Updated++
		c.log.Info("已更新群名称", "group", g.OpenID, "old", g.Name, "new", name)
	}

	c.log.Info("群名称刷新完成",
		"总数", st.Total, "更新", st.Updated, "未变化", st.Same,
		"空名", st.Empty, "手工跳过", st.Manual, "失败", st.Failed,
		"无权限", st.Forbidden, "提前结束", st.Aborted)
	c.reportRefresh(requester, st)
}

// reportRefresh 把结果私聊给发起人。
//
// 主动消息可能发不出去（[active] 关掉、对方从没私聊过），所以结果
// 同时写日志——refresh 的产出已经在库里，汇报只是体验。
func (c *Core) reportRefresh(requester string, st refreshStats) {
	var b strings.Builder
	b.WriteString("群名称刷新完成\n")
	fmt.Fprintf(&b, "共 %d 个在群", st.Total)
	if st.Aborted {
		b.WriteString("（超时提前结束）")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "· 已更新 %d\n", st.Updated)
	fmt.Fprintf(&b, "· 未变化 %d\n", st.Same)
	if st.Manual > 0 {
		fmt.Fprintf(&b, "· 手工名跳过 %d\n", st.Manual)
	}
	if st.Empty > 0 {
		fmt.Fprintf(&b, "· 接口未返回名称 %d\n", st.Empty)
	}
	if st.Failed > 0 {
		fmt.Fprintf(&b, "· 失败 %d\n", st.Failed)
	}
	if st.Forbidden {
		b.WriteString("\n群信息接口对所有群都返回 11253：该接口仅对内邀白名单机器人开放，")
		b.WriteString("群名只能手工设置（/group name <openid> <名称>）。")
	}
	if st.Updated > 0 {
		b.WriteString("\n用 /group list 查看结果。")
	}

	if requester == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.sender.SendActive(ctx, host.SceneC2C, requester,
		host.Reply{Content: strings.TrimRight(b.String(), "\n")}); err != nil {
		c.log.Warn("刷新结果汇报失败（结果已写入数据库与日志）", "peer", requester, "err", err)
	}
}

// refreshBudget 给整批刷新一个总时限。
//
// 单个请求有自己的 8s 超时，正常情况下远用不到；总预算只为兜住
// "网络整体不可用"这种最坏情况，避免后台任务无限期挂着。
func refreshBudget(n int) time.Duration {
	d := time.Duration(n) * groupInfoTimeout
	if d < groupInfoTimeout {
		return groupInfoTimeout
	}
	if d > 10*time.Minute {
		return 10 * time.Minute
	}
	return d
}

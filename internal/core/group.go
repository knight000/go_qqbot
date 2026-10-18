package core

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

// 群准入闸门（ARCHITECTURE.md §8.8）。
//
// 位置很关键：它在**去重、落库、路由之前**。不在白名单里的群，消息不落 messages、
// 不进 processed_events、不触发任何插件——"不进入 bot 系统"是字面意思。
// 唯一的例外是 Bot 管理员的 /group 指令，否则新群永远无法在群内自助加入白名单。
//
// 登记（qq_groups）发生在判定之前：即便这个群会被丢弃，也要留下 openid，
// 否则管理员在私聊里既看不到它，也没法按 id 把它加进白名单。

// groupAccess 返回事件是否放行、该群的登记项，以及拒绝原因（用于轨迹）。
func (c *Core) groupAccess(ctx context.Context, ev *host.Event) (bool, store.Group, string) {
	if ev.Scene != host.SceneGroup || ev.PeerID == "" {
		return true, store.Group{}, ""
	}
	// 只有测试会构造没有配置的 Core；生产路径由 New() 保证 cfg 非空。
	if c.cfg == nil {
		return true, store.Group{}, ""
	}

	g, changed, err := c.store.ObserveGroup(ctx, ev.PeerID)
	if err != nil {
		// 准入判定依赖本地库：读不到就按未放行处理（fail-closed），
		// 与去重表故障时的策略一致——宁可这条消息不处理。
		c.log.Error("登记群失败，按未放行处理", "peer", ev.PeerID, "err", err)
		return false, store.Group{}, "群登记失败"
	}
	if changed {
		// 首次见到该群，或它刚从"已退出"恢复：两个时机都只发生一次，
		// 正好用来补建 per-peer 任务的群实例。退群事件丢失时这是自愈路径。
		c.expandPeerJobs(ctx, ev.PeerID)
		if c.cfg.GroupNameFetchEnabled() {
			// 异步取群名：群信息接口可能很慢，不能挡住第一条消息的路由。
			// 失败只影响显示名，不影响准入判定。
			go c.fetchGroupName(ev.PeerID)
		}
	}

	allowed, reason := c.evaluateGroup(g)
	return allowed, g, reason
}

// evaluateGroup 是准入判定的唯一实现，闸门与 /group status 共用，
// 避免"指令里说放行、实际却丢弃"这类不一致。
func (c *Core) evaluateGroup(g store.Group) (bool, string) {
	switch {
	case c.blacklistEnabled() && g.Mode == store.GroupModeDeny:
		return false, "黑名单"
	case c.whitelistEnabled() && g.Mode != store.GroupModeAllow:
		return false, "未加入白名单"
	}
	return true, ""
}

// 配置缺失只可能出现在测试里；默认值与配置文件一致（两个开关都开）。
func (c *Core) whitelistEnabled() bool { return c.cfg == nil || c.cfg.WhitelistEnabled() }
func (c *Core) blacklistEnabled() bool { return c.cfg == nil || c.cfg.BlacklistEnabled() }

// logGroupGateState 在启动时把准入状态说清楚。
//
// 白名单默认开启，而新库的名单是空的——这意味着升级后所有群消息都会被丢弃。
// 这条 warn 是排查"机器人怎么不理人了"的第一线索，必须在启动日志里出现。
func (c *Core) logGroupGateState(ctx context.Context) {
	if c.store == nil || !c.whitelistEnabled() {
		return
	}
	s, err := c.store.CountGroups(ctx)
	if err != nil {
		c.log.Warn("读取群准入状态失败", "err", err)
		return
	}
	if s.Allowed == 0 {
		c.log.Warn("群白名单已开启但没有允许的群，所有群消息都会被丢弃",
			"已登记群", s.InUse(),
			"处理", "私聊 /group allow <group_openid>，或由 Bot 管理员在群内发 /group allow")
		return
	}
	c.log.Info("群准入已启用", "白名单", s.Allowed, "已登记群", s.InUse(),
		"已退出", s.Left, "黑名单开关", c.blacklistEnabled())
}

// groupControlExempt 判断这条被拦下的消息是否可以例外放行到核心指令。
//
// 三个条件缺一不可：群不在黑名单（黑名单是硬阻断，只能从私聊解除）、
// 内容确实是 /group 指令、发送者是 Bot 管理员或所有者。
// 普通用户在未授权群里发的任何内容——包括 /group——都不会得到任何回应。
func (c *Core) groupControlExempt(ctx context.Context, ev *host.Event, g store.Group) bool {
	if g.Mode == store.GroupModeDeny || !ev.IsMessage() {
		return false
	}
	if !isGroupControlCommand(ev.Content) {
		return false
	}
	return c.authorize(ctx, ev).IsBotAdmin()
}

// isGroupControlCommand 只认独立的 /group 指令，`/groups` 之类不算。
func isGroupControlCommand(content string) bool {
	f := strings.Fields(strings.TrimSpace(content))
	return len(f) > 0 && strings.EqualFold(f[0], "/group")
}

const groupInfoTimeout = 8 * time.Second

var (
	// ErrGroupInfoForbidden 表示群信息接口拒绝访问（11253：仅白名单机器人可用，
	// 需要官方内邀）。它不是配置错误，只是这个可选增强不可用。
	ErrGroupInfoForbidden = errors.New("群信息接口无访问权限(11253)")
	errNoGroupInfoSource  = errors.New("未配置群信息接口")
)

// groupInfoSource 是核心取群信息的外部依赖。
//
// 抽成接口只有一个目的：让"入群取名字"和"批量刷新群名"这两条逻辑
// 能在没有真实 API、没有网络的情况下被测试——它们全是分支逻辑，不值得靠人肉验证。
type groupInfoSource interface {
	GroupInfo(ctx context.Context, groupOpenID string) (api.GroupInfo, error)
}

// lookupGroupName 调接口取群名。
//
// 单次调用自己的超时在这里统一兜住，调用方只管传上下文：批量刷新时
// 一个卡死的请求绝不能吃掉整批的预算。
//
// 返回空名也不算错误：平台可能有这个群但不给名字。是否写库由调用方决定——
// 把空名写进去会擦掉已有的名字。
func (c *Core) lookupGroupName(ctx context.Context, openid string) (string, error) {
	if c.api == nil {
		return "", errNoGroupInfoSource
	}
	ctx, cancel := context.WithTimeout(ctx, groupInfoTimeout)
	defer cancel()

	info, err := c.api.GroupInfo(ctx, openid)
	if err != nil {
		if strings.Contains(err.Error(), "11253") {
			return "", ErrGroupInfoForbidden
		}
		return "", err
	}
	return strings.TrimSpace(info.GroupName), nil
}

// fetchGroupName 尝试用官方接口补全群名（首次见到新群时异步调用一次）。
//
// 该接口（GET /v2/groups/{openid}/info）目前仅对白名单机器人开放，
// 普通机器人调用会得到 11253；这不是错误配置，只是一个可选增强。
// 失败后不重试：群名可以随时用 /group name 手工设置，而每条新群消息都重试
// 只会白白消耗 30 QPM 的配额。需要批量重试时走 /group refresh。
func (c *Core) fetchGroupName(openid string) {
	ctx := context.Background()
	name, err := c.lookupGroupName(ctx, openid)
	switch {
	case errors.Is(err, ErrGroupInfoForbidden):
		c.log.Info("群信息接口无访问权限（11253），群名留空；可用 /group name 设置",
			"group", openid)
		return
	case err != nil:
		c.log.Debug("获取群名称失败，群名留空", "group", openid, "err", err)
		return
	case name == "":
		return
	}
	if err := c.store.SetGroupName(ctx, openid, name, "api"); err != nil {
		c.log.Warn("写入群名称失败", "group", openid, "err", err)
		return
	}
	c.log.Info("已记录群名称", "group", openid, "name", name)
}

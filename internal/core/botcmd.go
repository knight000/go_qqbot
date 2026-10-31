package core

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// /bot 是**进程级**的 Bot 管理指令（ADR-0008 决策 9/15/16）。
//
// 与 /reset 的分界线：/reset 管「一个 Bot 里的数据」，/bot 管「进程里有哪几个
// Bot、谁在连」。所以它只认 Core 所有者——Bot 自己库里的 admins 够管那一个
// Bot，够不到"把别人停掉"（决策 9）。
//
// 本版本刻意**不做** /bot enable|disable：配置文件是存在的唯一事实源，命令级
// 开关会立刻制造"运行态与配置不一致"，而写回文件会重排用户注释。停用一个 Bot
// 就是把配置里 enabled 改成 false 再 /bot reload。
//
// /reload 的语义不变（整进程 exec 重启）：数据目录、插件二进制路径这类全局
// 配置改了必然要重启，假装能热改只会得到半生效状态（决策 15）。

const botUsage = `用法：
/bot list                     列出进程内的 Bot、状态与是否已认领
/bot reload                   重读配置文件，按 appid 差异增/删/改（只动有变化的）
/bot reload <appid>           只重连这一个，不重读整份配置
/bot remove <appid> --purge   清空该 Bot 的全部业务数据（要确认码）

/bot remove --purge 的前置条件：该 appid 必须已经不在配置里。
先删配置条目 → /bot reload → 再 purge；否则清空数据后它会被下一次 reload
重新拉起来，等于把租户数据清空后继续对外服务。
全局配置（数据目录、插件路径、日志级别）改了仍然要用 /reload 重启进程。`

// BotView 是一个 Bot 的对外快照：/bot list 只读它，不去猜容器内部状态。
type BotView struct {
	AppID string
	// CertType 是认证类型（unverified / personal / enterprise）。
	CertType string
	// State 是状态机的四个值之一；空串表示读不出来（库坏了）。
	State string
	// SuspendReason 只在 State=suspended 时有意义：config_disabled / config_gone。
	SuspendReason string
	// Claimed 表示该 Bot 的 admins 非空（已被认领）。
	Claimed bool
	// Enabled 是配置里的值；Running 表示这个进程此刻是否真的连着。
	Enabled bool
	Running bool
	// Err 非空表示这个 Bot 上次装配失败（锁被占、开库失败…）。
	Err string
}

// BotReloadSummary 是一次 /bot reload 的账。数字必须能对上话：
// 「新增 1、重启 1、停用 1」是给人确认"刚才发生了什么"的唯一输出。
type BotReloadSummary struct {
	Added     int
	Restarted int
	Stopped   int
	Unchanged int
	// Deferred 是"本该重连但因为它是当前处理这条指令的 Bot 而没动"的 appid。
	// 自己不能在自己的消息处理里关掉自己（会丢掉这条回复），所以只能让人
	// 换个 Bot 再发一次，或者用 /reload。
	Deferred []string
	// Failed 是装配失败的 appid（其余 Bot 不受影响，失败只记账）。
	Failed []string
}

// BotHotService 是装配层（internal/instance 的容器）暴露的热操作入口。
//
// core 不能 import internal/instance（那会形成装配层 ↔ 核心的循环依赖），
// 所以这里只声明核心需要的最小面，由装配点注入。nil 表示本进程没有装配热
// 操作（单测、精简构建），/bot 会如实回一句"未装配"，而不是假装成功。
type BotHotService interface {
	// Snapshot 返回当前进程知道的所有 Bot（含已从配置消失、数据仍在的）。
	Snapshot(ctx context.Context) ([]BotView, error)
	// ReloadAll 重读配置文件按 appid 求差集。self 是发指令的 Bot：它自己
	// 不在这一次里被停/重启，见 BotReloadSummary.Deferred。
	ReloadAll(ctx context.Context, self string) (BotReloadSummary, error)
	// ReloadOne 只重连 appid 这一个，不重读配置。self 同 ReloadAll。
	ReloadOne(ctx context.Context, self, appid string) error
	// PurgeStats 返回 purge 会删掉多少行（预览用，不动数据）。
	PurgeStats(ctx context.Context, appid string) (store.ResetCounts, error)
	// Purge 执行 R3 全量清理，返回删掉的行数与媒体文件数。
	Purge(ctx context.Context, appid string) (store.ResetCounts, int, error)
}

var (
	// ErrBotStillConfigured 表示该 appid 还在配置里，purge 被拒绝。
	ErrBotStillConfigured = errors.New("该 Bot 仍在配置里")
	// ErrBotSelfOp 表示这条指令要求对"正在处理它的 Bot"动手。
	ErrBotSelfOp = errors.New("不能对当前 Bot 执行这条指令")
)

// botPurgeLevel 是 /bot remove --purge 在 reset_audit 里的 level 取值。
//
// 复用 R3 的确认码机制（60 秒、绑 actor+peer+level、一次性），只是换一个
// level 名字：它和 /reset all 的冷却、语义都不同，混用会让 /reset status
// 里出现一条谁也解释不清的记录。
const botPurgeLevel = "bot_purge"

func (c *Core) handleBot(ctx context.Context, ev *host.Event) {
	if !c.isCoreOwner(ev) {
		// 与 /reload、/upgrade 同一条口径：核心指令明确回一句，不静默。
		c.replyText(ctx, ev, "该指令仅 Core 所有者可用。")
		return
	}
	if c.hot == nil {
		c.replyText(ctx, ev, "热操作未装配：本进程不支持 /bot（数据目录与插件路径改了请用 /reload）。")
		return
	}

	fields := strings.Fields(strings.TrimSpace(ev.Content))
	sub := ""
	if len(fields) > 1 {
		sub = strings.ToLower(fields[1])
	}
	args := argsAfter(fields, 2)

	switch sub {
	case "", "help":
		c.replyText(ctx, ev, botUsage)
	case "list":
		c.botList(ctx, ev)
	case "reload":
		c.botReload(ctx, ev, args)
	case "remove":
		c.botRemove(ctx, ev, args)
	default:
		c.replyText(ctx, ev, "未知的子命令。\n"+botUsage)
	}
}

func (c *Core) botList(ctx context.Context, ev *host.Event) {
	views, err := c.hot.Snapshot(ctx)
	if err != nil {
		c.log.Error("读取 Bot 快照失败", "err", err)
		c.replyText(ctx, ev, "读取 Bot 快照失败，请查看日志："+err.Error())
		return
	}
	c.replyText(ctx, ev, formatBotList(views))
}

// formatBotList 一行一个 Bot，末尾一行汇总。
func formatBotList(views []BotView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Bot 列表（%d 个）", len(views))
	var running, claimed, failing, suspended, gone int
	for _, v := range views {
		state := v.State
		if state == "" {
			state = "unknown"
		}
		if v.State == string(BotStateSuspended) && v.SuspendReason != "" {
			state += "(" + v.SuspendReason + ")"
		}
		b.WriteString("\n")
		b.WriteString(v.AppID)
		fmt.Fprintf(&b, "  %s  %s  %s  enabled=%t", v.CertType, state, claimedLabel(v.Claimed), v.Enabled)
		if !v.Running {
			b.WriteString("  未连接")
		}
		if v.Err != "" {
			b.WriteString("  错误=" + v.Err)
		}

		if v.Running {
			running++
		}
		if v.Claimed {
			claimed++
		}
		if v.State == string(BotStateFailing) {
			failing++
		}
		if v.State == string(BotStateSuspended) {
			suspended++
			if v.SuspendReason == SuspendReasonConfigGone {
				gone++
			}
		}
	}
	fmt.Fprintf(&b, "\n汇总：连接 %d、已认领 %d、failing %d、suspended %d（其中配置外 %d）",
		running, claimed, failing, suspended, gone)
	return b.String()
}

func claimedLabel(claimed bool) string {
	if claimed {
		return "已认领"
	}
	return "未认领"
}

func (c *Core) botReload(ctx context.Context, ev *host.Event, args []string) {
	if len(args) > 1 {
		c.replyText(ctx, ev, botUsage)
		return
	}
	if len(args) == 1 {
		appid := args[0]
		if err := c.hot.ReloadOne(ctx, c.botID, appid); err != nil {
			c.replyText(ctx, ev, botReloadErrorText(appid, err))
			return
		}
		c.log.Warn("执行了单 Bot 重连", "appid", appid, "by", ev.SenderID)
		c.replyText(ctx, ev, fmt.Sprintf("已重连 %s：本次只动这一个，其余 Bot 的连接与令牌缓存不变。", appid))
		return
	}

	sum, err := c.hot.ReloadAll(ctx, c.botID)
	if err != nil {
		c.log.Error("重读配置失败", "err", err)
		c.replyText(ctx, ev, "重读配置失败，未改动任何 Bot："+err.Error())
		return
	}
	c.log.Warn("执行了配置热重载",
		"added", sum.Added, "restarted", sum.Restarted,
		"stopped", sum.Stopped, "unchanged", sum.Unchanged, "by", ev.SenderID)
	c.replyText(ctx, ev, formatReloadSummary(sum))
}

func botReloadErrorText(appid string, err error) string {
	switch {
	case errors.Is(err, ErrBotSelfOp):
		return fmt.Sprintf("不能重连 %s：它就是当前处理这条指令的 Bot。"+
			"在自己的消息处理里关掉自己会丢掉这条回复，请在另一个 Bot 上发 /bot reload %s，或用 /reload 重启进程。",
			appid, appid)
	case errors.Is(err, ErrBotStillConfigured):
		return err.Error()
	}
	return fmt.Sprintf("重连 %s 失败：%v", appid, err)
}

func formatReloadSummary(sum BotReloadSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "热重载完成：新增 %d、重启 %d、停用 %d", sum.Added, sum.Restarted, sum.Stopped)
	if sum.Unchanged > 0 {
		fmt.Fprintf(&b, "（其余 %d 个未变）", sum.Unchanged)
	}
	b.WriteString("。")
	if len(sum.Failed) > 0 {
		b.WriteString("\n未能启动：" + strings.Join(sum.Failed, "、") + "（其余 Bot 不受影响，详见日志）")
	}
	if len(sum.Deferred) > 0 {
		b.WriteString("\n本 Bot 的变更未执行：" + strings.Join(sum.Deferred, "、") +
			"——它就是当前处理这条指令的 Bot。请在另一个 Bot 上发 /bot reload <appid>，或 /reload 重启进程。")
	}
	return b.String()
}

// botRemove 处理 /bot remove <appid> --purge [确认码]。
//
// 只支持 --purge：不带它的 /bot remove 会让人以为"删掉配置条目"就够了，
// 而那件事已经是编辑配置文件本身。数据保留的停用由删条目 + /bot reload 完成。
func (c *Core) botRemove(ctx context.Context, ev *host.Event, args []string) {
	appid, purge, code, ok := parseBotRemoveArgs(args)
	if !ok || appid == "" {
		c.replyText(ctx, ev, "用法：/bot remove <appid> --purge\n"+botUsage)
		return
	}
	if !purge {
		c.replyText(ctx, ev, "只支持 /bot remove <appid> --purge。\n"+
			"想让一个 Bot 停止但保留数据：删掉配置条目后 /bot reload。")
		return
	}
	if appid == c.botID {
		c.replyText(ctx, ev, "不能在当前 Bot 上删自己：这条回复本身就走它自己的库与令牌。\n"+
			"请在另一个 Bot 上执行，或用 /reload 重启进程后再处理它。")
		return
	}

	stats, err := c.hot.PurgeStats(ctx, appid)
	if err != nil {
		if errors.Is(err, ErrBotStillConfigured) {
			c.replyText(ctx, ev, fmt.Sprintf("拒绝清理 %s：它仍在配置里。\n"+
				"先删掉配置条目并 /bot reload，确认它已停用，再执行 /bot remove %s --purge。"+
				"\n理由：配置里还在的话，purge 完下一次 reload 会把它重新拉起来，等于清空数据后继续对外服务。",
				appid, appid))
			return
		}
		c.replyText(ctx, ev, fmt.Sprintf("读取 %s 的待清理数据失败：%v", appid, err))
		return
	}

	if code == "" {
		// 只预览 + 发码，绝不执行（与 /reset 的 R3 同一条机制）。
		cc, err := newResetCode()
		if err != nil {
			c.log.Error("生成确认码失败", "err", err)
			c.replyText(ctx, ev, "生成确认码失败，请查看日志。")
			return
		}
		if _, err := c.store.OpenResetPending(ctx, ev.SenderID, appid, botPurgeLevel, cc, resetCodeTTL); err != nil {
			c.log.Error("写入待确认清理失败", "appid", appid, "err", err)
			c.replyText(ctx, ev, "写入审计失败，已中止。")
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "将清空 %s 的全部业务数据，不可恢复：%s。\n", appid, purgeStatsText(stats))
		b.WriteString("保留：管理员、群名单、网关凭证。\n")
		fmt.Fprintf(&b, "%d 秒内回复 /bot remove %s --purge %s 确认。",
			int(resetCodeTTL.Seconds()), appid, cc)
		c.replyText(ctx, ev, b.String())
		return
	}

	rec, found, err := c.store.PendingReset(ctx, ev.SenderID, appid, botPurgeLevel)
	if err != nil {
		c.log.Error("读取待确认清理失败", "appid", appid, "err", err)
		c.replyText(ctx, ev, "读取审计失败，请查看日志。")
		return
	}
	if !found || time.Now().After(rec.ExpiresAt) {
		c.replyText(ctx, ev, "没有待确认的清理请求，或确认码已过期。请重新发送 /bot remove "+appid+" --purge。")
		return
	}
	if subtle.ConstantTimeCompare([]byte(rec.Code), []byte(code)) != 1 {
		c.replyText(ctx, ev, "确认码不正确。")
		return
	}

	// 配置在"发码"到"确认"这 60 秒里可能又被加回去：执行前必须再判一次。
	// 这里复用 PurgeStats 的同一道校验，避免出现"预览说可以、执行时才发现不行"。
	if _, err := c.hot.PurgeStats(ctx, appid); err != nil {
		_ = c.store.CloseReset(ctx, rec.ID, store.ResetStatusCancelled, "配置里又出现了")
		c.replyText(ctx, ev, fmt.Sprintf("已中止：%v。", err))
		return
	}

	counts, files, purgeErr := c.hot.Purge(ctx, appid)
	status, detail := store.ResetStatusDone, purgeStatsText(counts)
	if purgeErr != nil {
		status = store.ResetStatusFailed
		detail = "失败: " + purgeErr.Error()
	}
	if err := c.store.CloseReset(ctx, rec.ID, status, detail); err != nil {
		c.log.Warn("更新清理审计失败", "appid", appid, "err", err)
	}
	if purgeErr != nil {
		c.log.Error("执行 Bot 清理失败", "appid", appid, "err", purgeErr)
		c.replyText(ctx, ev, fmt.Sprintf("清理 %s 失败，已中止：%v", appid, purgeErr))
		return
	}

	c.log.Warn("执行了 Bot 数据清理", "appid", appid, "by", ev.SenderID, "detail", detail)
	c.replyText(ctx, ev, fmt.Sprintf("已清理 %s：%s（媒体文件 %d 个）。\n"+
		"管理员、群名单与网关凭证均保留；配置条目可以之后再加回来。",
		appid, purgeStatsText(counts), files))
}

// parseBotRemoveArgs 从参数里挑出 appid、--purge 与 6 位确认码，顺序无关。
//
// 与 /reset 的 splitResetArgs 同一条理由：三者形态天然可分（appid 是 slug、
// 确认码是 6 位数字、--purge 是固定词），按位置解析只会要求用户记顺序。
func parseBotRemoveArgs(args []string) (appid string, purge bool, code string, ok bool) {
	for _, a := range args {
		switch {
		case strings.EqualFold(a, "--purge"):
			purge = true
		case code == "" && isResetCode(a):
			code = a
		case appid == "":
			appid = a
		default:
			return "", false, "", false
		}
	}
	return appid, purge, code, true
}

func purgeStatsText(c store.ResetCounts) string {
	return fmt.Sprintf("消息 %d 条、媒体 %d 个、插件会话 %d 条、任务记录 %d 条、去重记录 %d 条、身份映射 %d 条、会话接管 %d 个",
		c.Messages, c.MediaRows, c.KVRows, c.JobRuns, c.Events, c.Identities, c.Sessions)
}

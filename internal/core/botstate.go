package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// Bot 生命周期状态机（ADR-0008 决策 8）：四态，**没有 banned**。
//
// 平台没有查询 Bot 状态的接口：被封禁、密钥失效、网络抽风在外部表现上是同一类
// 错误，所以它们统一归入 failing，由人看日志判断。硬要分出一个 banned，只会
// 让"状态"变成一个我们猜的结论。
type BotState string

const (
	// BotStatePendingClaim 是"还没人认领"：只有 /claim 能穿过去。
	BotStatePendingClaim BotState = "pending_claim"
	// BotStateActive 是唯一正常收发事件的状态。
	BotStateActive BotState = "active"
	// BotStateSuspended 只能由配置 enabled=false 进入，也只能人工解除。
	BotStateSuspended BotState = "suspended"
	// BotStateFailing 是"连续鉴权类失败"，靠退避重连恢复。
	BotStateFailing BotState = "failing"
)

// StateKeyBotState 是 bot_state 表里存 Bot 状态的键。
//
// 键名只在这里出现：读写都走 LoadBotState / SaveBotState，别处再拼一次字符串
// 就会出现"写入用 bot.status、读取用 bot_state"这种只在排障时发现的错位。
const StateKeyBotState = "bot_state"

// StateKeyBotSuspendReason 是 bot_state 表里存"为什么停用"的键。
//
// suspended 有两个来源：配置里 enabled=false，以及配置里**没有这一条**了
// （#68 的热删除，数据保留）。两者对闸门是同一件事——都全丢事件——但对人
// 完全不是：前者是"我关的"，后者是"它从配置里消失了，可以 purge"。状态仍是
// 四态里的 suspended（ADR-0008 决策 8），不新造第五个状态。
const StateKeyBotSuspendReason = "bot_suspend_reason"

const (
	// SuspendReasonConfigDisabled 是配置 enabled = false。
	SuspendReasonConfigDisabled = "config_disabled"
	// SuspendReasonConfigGone 是配置里已经删掉了这一条。
	SuspendReasonConfigGone = "config_gone"
)

// SaveSuspendReason 记录停用原因。只写 bot_state，不动状态本身。
func SaveSuspendReason(ctx context.Context, db *store.DB, reason string) error {
	return db.PutState(ctx, StateKeyBotSuspendReason, reason)
}

// LoadSuspendReason 读停用原因；没有记录时返回空串。
func LoadSuspendReason(ctx context.Context, db *store.DB) (string, error) {
	v, ok, err := db.GetState(ctx, StateKeyBotSuspendReason)
	if err != nil || !ok {
		return "", err
	}
	return v, nil
}

// authFailureThreshold 是进入 failing 的连续失败次数。
// 偶发一次鉴权错误（例如平台抖动、令牌刚好过期）不该让 Bot 停摆。
const authFailureThreshold = 3

// authErrorCodes 是"重试无意义、且指向身份不被认可"的业务码。
//
// 100016（invalid appid or secret）既出现在取 token 上，也会出现在发送消息上：
// 平台侧密钥失效、AppID 被下架、被封禁的外部表现都是它。新增码必须有现场日志
// 支撑再往这张表里加——凭猜测扩表会把网络抖动误判成 failing。
var authErrorCodes = map[int]struct{}{
	100016: {},
}

// Valid 表示这个值能不能落库/回读。写库前校验，避免把拼错的状态写进去
// 之后每一次读都变成"未知状态"。
func (s BotState) Valid() bool {
	switch s {
	case BotStatePendingClaim, BotStateActive, BotStateSuspended, BotStateFailing:
		return true
	}
	return false
}

// LoadBotState 读这个 Bot 库里的状态。
//
// 没有行时按 admins 是否为空推导：空 → pending_claim，否则 active。推导结果
// **不落库**——配置与 admins 才是事实源，落库只会制造第二份真相（ADR-0008
// 决策 8 拒绝全局注册表的同一条理由）。
func LoadBotState(ctx context.Context, db *store.DB) (BotState, error) {
	v, ok, err := db.GetState(ctx, StateKeyBotState)
	if err != nil {
		return "", err
	}
	if !ok {
		n, err := db.CountAdmins(ctx)
		if err != nil {
			return "", fmt.Errorf("推导 Bot 状态: %w", err)
		}
		if n == 0 {
			return BotStatePendingClaim, nil
		}
		return BotStateActive, nil
	}
	st := BotState(v)
	if !st.Valid() {
		return "", fmt.Errorf("bot_state[%s] 的值未知: %q", StateKeyBotState, v)
	}
	return st, nil
}

// SaveBotState 落一条状态。空状态与未知状态都拒绝写。
func SaveBotState(ctx context.Context, db *store.DB, st BotState) error {
	if !st.Valid() {
		return fmt.Errorf("非法 Bot 状态: %q", st)
	}
	return db.PutState(ctx, StateKeyBotState, string(st))
}

// ReconcileBotState 在启动时把库里的状态与配置对齐，返回对齐后的状态。
//
// 规则只有两条：
//   - enabled=false → suspended（配置说停用，库里存什么都得让位）；
//   - enabled=true 而库里是 suspended → 解除，回到 pending_claim/active
//     （"人工解除"就是配置改回 true，重启即生效）。
//
// 其余状态原样保留：failing 要不要继续退避，由运行期决定，启动不该擅自清掉。
func ReconcileBotState(ctx context.Context, db *store.DB, enabled bool) (BotState, error) {
	st, err := LoadBotState(ctx, db)
	if err != nil {
		return "", err
	}
	if !enabled {
		if st != BotStateSuspended {
			if err := SaveBotState(ctx, db, BotStateSuspended); err != nil {
				return st, err
			}
			return BotStateSuspended, nil
		}
		return st, nil
	}
	if st != BotStateSuspended {
		// 已是启用态：残留的停用原因必须清掉，否则排障时它会一直指向一次
		// 早已结束的停用（"配置里没了"是最容易被误读成当前事实的一句话）。
		_ = db.DeleteState(ctx, StateKeyBotSuspendReason)
		return st, nil
	}
	// 解除停用：admins 非空即 active，否则又是"等人认领"。
	n, err := db.CountAdmins(ctx)
	if err != nil {
		return st, fmt.Errorf("解除停用时推导 Bot 状态: %w", err)
	}
	next := BotStatePendingClaim
	if n > 0 {
		next = BotStateActive
	}
	if err := SaveBotState(ctx, db, next); err != nil {
		return st, err
	}
	// 停用原因随解除一起消失：它只描述"上一次为什么停下"。
	if err := db.DeleteState(ctx, StateKeyBotSuspendReason); err != nil {
		return next, err
	}
	return next, nil
}

// BotState 返回当前状态（进程内缓存）。
//
// 缓存是必须的：这是每条事件都要过的第一道判定，不能来一条消息查一次库。
// 状态只由本进程的迁移改写（认领、鉴权失败、重连成功、启动对齐），所以缓存
// 与库不会长期分叉。
func (c *Core) BotState(ctx context.Context) (BotState, error) {
	if c.store == nil {
		// 没有库的 Core 只出现在单测里：没有状态可判，按 active 放行。
		return BotStateActive, nil
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.loadStateLocked(ctx)
}

// loadStateLocked 调库读取（调用方须持 stateMu）。
func (c *Core) loadStateLocked(ctx context.Context) (BotState, error) {
	if c.state != "" && c.stateKnown {
		return c.state, nil
	}
	st, err := LoadBotState(ctx, c.store)
	if err != nil {
		return "", err
	}
	c.state, c.stateKnown = st, true
	return st, nil
}

// transition 把状态改到 to 并打一条带 bot id 的日志。
//
// 状态没变就什么都不做：重连成功、每次认领都会调它，重复打日志会把真正的
// 变更淹掉。写库失败只 Warn：状态机在内存里继续走，比"因为写不进库所以
// 永远停在旧状态"正确。
func (c *Core) transition(ctx context.Context, to BotState, reason string) {
	c.stateMu.Lock()
	from, err := c.loadStateLocked(ctx)
	if err != nil {
		// 读不到就当未知态迁移：宁可写一条显式状态，让下次启动有据可依。
		c.log.Warn("读取 Bot 状态失败，按当前状态迁移", "to", to, "err", err)
	}
	if from == to {
		c.stateMu.Unlock()
		return
	}
	c.state, c.stateKnown = to, true
	c.stateMu.Unlock()

	if err := SaveBotState(ctx, c.store, to); err != nil {
		c.log.Warn("Bot 状态落库失败", "to", string(to), "err", err)
	}
	c.log.Info("Bot 状态变更",
		"bot", c.botID, "from", string(from), "to", string(to), "reason", reason)
}

// NoteConnected 由接入层在 READY/RESUMED 时调用：只有握手成功才算"重连成功"。
//
// 只把 failing 翻回 active。pending_claim / suspended 不会因为连上就变——
// 前者等认领，后者等人改配置。
func (c *Core) NoteConnected() {
	st, err := c.BotState(context.Background())
	if err != nil {
		return
	}
	if st != BotStateFailing {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.transition(ctx, BotStateActive, "重连成功")
	c.resetAuthFailures()
}

// noteSendResult 记录一次出站结果，维护"连续鉴权失败"计数。
//
// 只有鉴权类业务码才计数，也只有它才谈得上"连续"：网络超时、429 都是可恢复
// 的，掺进同一个计数器会让退避重连变成对网络抖动的误判。成功一次即清零。
func (c *Core) noteSendResult(ctx context.Context, err error) {
	if err == nil {
		c.resetAuthFailures()
		return
	}
	if !isAuthFailure(err) {
		return
	}
	c.stateMu.Lock()
	c.authFails++
	n := c.authFails
	if n >= authFailureThreshold {
		c.authFails = 0
	}
	c.stateMu.Unlock()
	if n < authFailureThreshold {
		return
	}
	c.enterFailing(ctx, fmt.Sprintf("连续 %d 次鉴权类发送失败", n))
}

func (c *Core) resetAuthFailures() {
	c.stateMu.Lock()
	c.authFails = 0
	c.stateMu.Unlock()
}

// enterFailing 进 failing 并主动断一条连接：不走这一步，网关那条链路可能
// 还是"通着"的，退避重连永远没机会发生。
func (c *Core) enterFailing(ctx context.Context, reason string) {
	c.transition(ctx, BotStateFailing, reason)
	if c.reconnect == nil {
		return
	}
	if err := c.reconnect.Reconnect(); err != nil {
		// 没有活着的连接是常态（网关正在退避），不是错误。
		c.log.Debug("进入 failing 后未能主动断开连接", "err", err)
	}
}

// isAuthFailure 判定一次出站错误是不是"这个 Bot 的身份不被认可"。
func isAuthFailure(err error) bool {
	var be *api.BusinessError
	if !errors.As(err, &be) {
		return false
	}
	_, ok := authErrorCodes[be.Code]
	return ok
}

// botGate 是事件入口的第一道闸门，**只判 Bot 状态**（ADR-0008 决策 7）。
//
// 返回 true 表示放行。非 active 时：
//   - pending_claim 只放行 /claim（其余丢弃，且不落库、不进幂等表）；
//   - suspended / failing 全丢。
//
// 这里不判来源：认证类型决定"能触及谁"，由平台在推送前执行，不在允许范围内的
// 会话其事件根本到不了核心。核心持有来源白名单只会得到一份与平台不同步的副本。
func (c *Core) botGate(ctx context.Context, ev *host.Event) bool {
	if c.store == nil {
		return true
	}
	st, err := c.BotState(ctx)
	if err != nil {
		// fail-closed：状态读不出来时不处理事件，但要在日志里说清是哪一步坏的。
		c.countDrop(BotStateFailing)
		c.log.Error("读取 Bot 状态失败，丢弃事件", "bot", c.botID, "event", ev.ID, "err", err)
		return false
	}
	switch st {
	case BotStateActive:
		return true
	case BotStatePendingClaim:
		if isClaimCommand(ev) {
			return true
		}
	}
	c.countDrop(st)
	c.trace.StateDropped(ev, st)
	return false
}

// countDrop 按状态计数。不逐条打日志：一条被丢弃的事件没有任何可行动信息，
// 而 pending_claim 的 Bot 在认领之前会收到平台推来的每一条消息。
func (c *Core) countDrop(st BotState) {
	c.stateMu.Lock()
	if c.dropped == nil {
		// 直接构造 Core 的用例（不经过 New）没有初始化这张表。
		c.dropped = make(map[BotState]int64, 4)
	}
	c.dropped[st]++
	n := c.dropped[st]
	c.stateMu.Unlock()
	// 只在 1 与每 100 条时留一行：数量级变化足够判断"是不是一直没人认领"。
	if n == 1 || n%100 == 0 {
		c.log.Debug("Bot 非 active，丢弃事件", "bot", c.botID, "state", string(st), "dropped", n)
	}
}

// DroppedByState 返回按状态累计的丢弃条数（运维与测试的唯一读法）。
func (c *Core) DroppedByState() map[BotState]int64 {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	out := make(map[BotState]int64, len(c.dropped))
	for k, v := range c.dropped {
		out[k] = v
	}
	return out
}

// isClaimCommand 判定这条事件是不是 /claim：闸门与 handleReserved 必须用同一
// 套判定，两处不一致就会出现"闸门放行了但没人处理"或反过来的黑洞。
func isClaimCommand(ev *host.Event) bool {
	if !ev.IsMessage() {
		return false
	}
	content := strings.TrimSpace(ev.Content)
	if !strings.HasPrefix(content, "/") {
		return false
	}
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return false
	}
	return strings.EqualFold(fields[0], "/claim")
}

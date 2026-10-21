package gateway

// 平台关闭码决定"下一步该做什么"，而不是只决定日志里写什么。
//
// 官方口径（bot.qq.com/wiki/develop/api-v2/dev-prepare/event-emit/websocket.html）：
// 4006/4007 是不可 Resume 的 session 故障，必须重新 Identify；4009 相反，
// 官方明确要求 Resume。把这些映射写成本文件的一张表，是因为历史上
// 4006/4007 只被包了一层错误文本，于是进程永远 Resume 同一个死 session——
// 重启和 /reset 都救不回来，只能人工改库。
//
// 表之外还有两条与时长有关的口径：4008 要等够 60s，退避复位只在
// "READY 之后活够 stableReadyWindow"时发生（见 backoff）。

import (
	"errors"
	"fmt"
	"time"
)

// closeAction 是关闭码要求的下一个动作。
type closeAction int

const (
	// actionReconnect 是未识别的关闭码：按退避重连，不动 session。
	actionReconnect closeAction = iota
	// actionReIdentify：session 已不可用，清掉 session_id/seq，下一轮 Identify。
	actionReIdentify
	// actionResume：连接过期（或平台要求重连），保留 session，下一轮 Resume。
	actionResume
	// actionInvalidToken：鉴权失败（4004），作废缓存 token 后重连。
	actionInvalidToken
	// actionRateLimited：发送过快（4008），至少等 60s 再连。
	actionRateLimited
	// actionFatal：永久性错误，停止重连并交出可读原因。
	actionFatal
)

func (a closeAction) String() string {
	switch a {
	case actionReIdentify:
		return "re-identify"
	case actionResume:
		return "resume"
	case actionInvalidToken:
		return "invalid-token"
	case actionRateLimited:
		return "rate-limited"
	case actionFatal:
		return "fatal"
	default:
		return "reconnect"
	}
}

// rateLimitedDelay 是 4008 之后的最小等待：平台给的是"发送过快，请降低频率"，
// 一秒后就撞回去几乎必然再次 4008，还会把 /gateway/bot 一起打。
const rateLimitedDelay = 60 * time.Second

// closeDisposition 是一个关闭码的处置结论。
type closeDisposition struct {
	action closeAction
	delay  time.Duration // 本轮重连的最短等待（目前只有 4008 用到）
	reason string        // 人能读懂的原因：进日志，也进 FatalError
}

func classifyClose(code int) closeDisposition {
	switch code {
	// —— 本地写错的东西：连多少次都一样 ——
	case 4001:
		return closeDisposition{action: actionFatal, reason: "opcode 不合法(4001)"}
	case 4002:
		return closeDisposition{action: actionFatal, reason: "payload 无法解码(4002)"}
	case 4010:
		return closeDisposition{action: actionFatal, reason: "分片参数不合法(4010)，检查 bot.shard_id / shard_total"}
	case 4011:
		return closeDisposition{action: actionFatal, reason: "连接数超过平台限制(4011)，检查是否有另一个实例连同一个 app"}
	case 4012:
		return closeDisposition{action: actionFatal, reason: "请求过多(4012)，平台拒绝了新连接"}
	case 4013:
		return closeDisposition{action: actionFatal, reason: "intents 不合法(4013)，检查 bot.intents 位掩码"}
	case 4014:
		return closeDisposition{action: actionFatal, reason: "intents 无权限(4014)，到开放平台申请该事件权限或从 bot.intents 里去掉"}

	// —— 鉴权 ——
	case 4004:
		return closeDisposition{action: actionInvalidToken, reason: "token 已失效(4004)"}

	// —— session 不可继续：必须重新 Identify ——
	case 4006:
		return closeDisposition{action: actionReIdentify, reason: "session 已失效(4006)"}
	case 4007:
		return closeDisposition{action: actionReIdentify, reason: "seq 错误(4007)"}

	// —— 需要等待 ——
	case 4008:
		return closeDisposition{action: actionRateLimited, delay: rateLimitedDelay, reason: "发送过快(4008)"}

	// —— 官方要求 Resume ——
	case 4009:
		return closeDisposition{action: actionResume, reason: "连接已过期(4009)"}

	case 4914:
		return closeDisposition{action: actionFatal, reason: "机器人已下架(4914)，只允许连接沙箱环境；请到开放平台重新上架"}
	case 4915:
		return closeDisposition{action: actionFatal, reason: "机器人已封禁(4915)，平台拒绝连接"}
	}

	// 4900–4913：平台内部错误，官方给的处置是重连（本地 session 可能已作废）。
	if code >= 4900 && code <= 4913 {
		return closeDisposition{action: actionReIdentify, reason: fmt.Sprintf("平台内部错误(%d)", code)}
	}

	return closeDisposition{action: actionReconnect}
}

// closeError 把"关闭码要求的动作"从读循环带到 Run：delay 是本轮的最短等待，
// reason 是给人看的原因。它不表示致命——致命用 FatalError。
type closeError struct {
	delay  time.Duration
	reason string
	err    error
}

func (e *closeError) Error() string { return fmt.Sprintf("%s: %v", e.reason, e.err) }

func (e *closeError) Unwrap() error { return e.err }

// FatalError 表示关闭码是永久性的：下架、封禁、intents 无权限、分片参数错误。
// 再连也不会好，所以 Run 不再重试，把它交给调用方（进程退出码 / systemd）。
type FatalError struct {
	Code   int
	Reason string
	Err    error
}

func (e *FatalError) Error() string { return fmt.Sprintf("%s: %v", e.Reason, e.Err) }

func (e *FatalError) Unwrap() error { return e.Err }

// closeDelay 返回该错误要求的最短等待；普通链路故障是 0。
func closeDelay(err error) time.Duration {
	var ce *closeError
	if errors.As(err, &ce) {
		return ce.delay
	}
	return 0
}

// 退避的三个口径。
const (
	// minBackoff 是稳定连接断开后的第一次等待。
	minBackoff = time.Second
	// maxBackoff 是单轮等待的上限。
	maxBackoff = 60 * time.Second
	// stableReadyWindow 是"这条连接算不算稳定"的门槛：READY/RESUMED 之后还得
	// 活过这么久，才把退避复位成 minBackoff。"连上 200ms 就断"（平台拒绝
	// token、握手被 reset）不复位——否则重连会退化成 1s 一次的快速撞墙。
	stableReadyWindow = 30 * time.Second
)

// backoff 是重连退避的状态机：Run 每轮先用 note 汇报上一条连接活了多久，
// 再用 next 取本轮该等多久。把它抽出来是为了让"短命连接不复位"这条
// 口径能被单测钉住，而不是埋在 Run 的循环里。
type backoff struct {
	d time.Duration
}

// note 记录上一条连接的寿命，只有稳定过才复位。
func (b *backoff) note(readyFor time.Duration) {
	if readyFor >= stableReadyWindow {
		b.d = minBackoff
	}
}

// next 返回本轮实际等待：带抖动的当前退避，且不小于 min；随后把基础退避翻倍。
func (b *backoff) next(min time.Duration) time.Duration {
	base := b.d
	if base < minBackoff {
		base = minBackoff
	}
	if min > base {
		base = min
	}
	wait := jitter(base)
	if wait < min {
		wait = min // 抖动只能加长，不能削掉平台要求的等待
	}
	b.d = base * 2
	if b.d > maxBackoff {
		b.d = maxBackoff
	}
	return wait
}

package host

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Session 让插件接管某个 peer 的**下一条消息**（ADR-0004）。
//
// 与 KV / PeerKV 的分工：KV 解决"存了什么"，Session 解决"谁接这条消息"。
// 登记之后，该 peer 的下一条 message 会直接投给本插件，其他插件（包括
// priority 更高的）这一条一次都不会被调用；投递后行即删除，一次性消费。
//
// 多步交互的写法是**在 Handle 里再次 Await**：每一步都是"登记下一步"，
// 不是"延长当前这一步"。没有续期标志，因为"谁有权延长"没有判断依据。
//
// 只能从 Handle 里调用：它需要一个 inbound 事件提供 peer 与 scene。
type Session interface {
	// Await 登记"该 peer 的下一条消息交给本插件"，并附带插件自定义 state
	// （建议 JSON，≤ MaxSessionStateBytes）。ttl <= 0 取 DefaultSessionTTL，
	// 超过 MaxSessionTTL 截到上限——TTL 是策略，算错不该让整条链路失败。
	//
	// 同 peer 已被其它插件持有 → ErrSessionBusy；同一插件重复 Await 是
	// "改 state、重新计时"。持有者已卸载时该行会被当作悬挂行替换掉。
	Await(ctx context.Context, ev *Event, state []byte, ttl time.Duration) error
}

const (
	// DefaultSessionTTL 是插件不指定 TTL 时的等待时长：足够走完一次"问一句、
	// 等回答"，又不至于让一条没人理的等待长期占住这个 peer。
	DefaultSessionTTL = 5 * time.Minute
	// MaxSessionTTL 是上限。超过就截断——不做"永不过期"的会话，
	// 它会把 peer 变成一把等待永不到来的输入的锁。
	MaxSessionTTL = time.Hour
	// MaxSessionStateBytes 是 state 的长度上限（8 KiB）。超限报错而不是截断：
	// 截断的 JSON 是坏数据，插件下次读到它只会得到更难查的错。
	MaxSessionStateBytes = 8 << 10
)

var (
	// ErrSessionBusy 表示该 peer 的会话已被其它插件持有。
	//
	// 定义在 host 包而不是实现包里：插件只 import host，哨兵放在实现包，
	// 插件的 errors.Is 永远拿不到它（理由同 ErrRenderTooLarge）。
	ErrSessionBusy = errors.New("host: 该 peer 的会话已被其它插件持有")

	// errSessionUnavailable 是"宿主没装配会话能力"，属于环境问题，
	// 故意不包装 ErrSessionBusy：插件据此该退回静态路由，而不是放弃等待。
	errSessionUnavailable = errors.New("会话能力未装配：宿主未注入 SessionBackend")
)

// SessionContext 出现在 Event.Session 上：非 nil 表示这条消息是对本插件
// Await 的回应，内容是 Await 时写进去的 state。此时 Command / Args 恒为空
// ——会话响应不是命令。
type SessionContext struct {
	Plugin string
	State  []byte
}

// SessionBackend 由 core 的适配器实现。插件永远看不到它：
// 版本、冲突判定、悬挂清理都在核心侧。
type SessionBackend interface {
	AwaitSession(ctx context.Context, plugin string, ev *Event, state []byte, ttl time.Duration) error
}

// sessionStore 是 Session 的实现：先在这一层做完契约校验，再交给后端。
//
// 校验放在这里而不是后端，是因为这是插件边界——与 renderer、KV 的
// "后端未初始化"同一层语义；core 侧拿到的永远是可执行的值。
type sessionStore struct {
	backend SessionBackend
	plugin  string
}

func (s *sessionStore) Await(ctx context.Context, ev *Event, state []byte, ttl time.Duration) error {
	if ev == nil || ev.PeerID == "" {
		return errors.New("没有 peer，无法登记会话")
	}
	if len(state) > MaxSessionStateBytes {
		return fmt.Errorf("会话 state 超过 %d 字节上限（%d 字节）", MaxSessionStateBytes, len(state))
	}
	switch {
	case ttl <= 0:
		ttl = DefaultSessionTTL
	case ttl > MaxSessionTTL:
		ttl = MaxSessionTTL
	}
	return s.backend.AwaitSession(ctx, s.plugin, ev, state, ttl)
}

// unavailableSession 是未注入后端时的替身：拒绝一切登记。
//
// 报错而不是静默成功：静默会让插件以为"等上了"，实际下一条消息照旧走静态
// 路由，表现为"有时候收得到有时候收不到"——那是最难查的一类故障。
type unavailableSession struct{}

func (unavailableSession) Await(context.Context, *Event, []byte, time.Duration) error {
	return errSessionUnavailable
}

// resolveSession 把注入值收敛成可直接调用的实现，nil 换成拒绝性替身。
func resolveSession(b SessionBackend, plugin string) Session {
	if b == nil {
		return unavailableSession{}
	}
	return &sessionStore{backend: b, plugin: plugin}
}

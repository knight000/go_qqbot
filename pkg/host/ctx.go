package host

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// 本文件是「Bot 上下文随 ctx 走」的两个半程（ADR-0008 决策 11）。
//
// 共享插件进程之后，同一个插件实例会先后（或同时）处理不同 Bot 的事件。
// 插件里存进结构体的那个 Host 是 Init 时的「那一个 Bot 的 Host」，所以
// **不能**拿它当作"当前 Bot"——并发下根本没有"当前"。唯一的当前性是调用
// 参数：每个 Handle / RunJob 的 ctx 里都带着这次调用属于哪个 Bot。
//
// 两个半程：
//   - WithHost / From：插件侧。核心在投递时把该 Bot 的 Host 放进 ctx，
//     插件用 From(ctx) 取回它（不传 ctx 的访问器没法换个 Bot 读）。
//   - WithBot / BotOf：核心侧。一个插件进程服务 N 个 Bot，而 host.Plugin 的
//     Init / Handle / RunJob 签名里没有 bot 参数，所以核心用 ctx 告诉共享句柄
//     「这次调用属于谁」。它们不出现在插件看到的线协议里。
//
// 与渲染器、会话同一条取舍：From 永不返回 nil，拿不到时给拒绝性替身。插件
// 因此不必写判空分支，而"用错上下文"会变成一条明确的错误而不是 panic。

// BotInfo 是「当前是哪个 Bot」：appid 是 Bot 的唯一标识（线协议里的 bot_id
// 就是它），CertType 是平台认证档位（unverified / personal / enterprise）。
//
// CertType 跟着 Bot 走而不是靠插件自己查配置：认证类型决定「这个 Bot 能触及
// 谁」，同一份插件代码在不同租户下的行为可能因此不同（ADR-0008 决策 6）。
type BotInfo struct {
	AppID    string
	CertType string
}

// IsZero 报告这是不是一个空身份。零值出现在两类场合：进程级调用
// （manifest / close 没有 Bot 上下文），以及调用方忘了带 Bot——后者是缺陷，
// 所以 From 的替身会把它记进日志。
func (b BotInfo) IsZero() bool { return b.AppID == "" }

type hostCtxKey struct{}

type botCtxKey struct{}

// WithHost 把 h 标成「本次调用的 Bot 的 Host」。
//
// 核心侧在投递给插件之前调用；插件侧也可以用它派生出绑定了别的 Host 的
// ctx（少见，但语义清晰：ctx 里放的是什么，From 就返回什么）。
func WithHost(ctx context.Context, h Host) context.Context {
	if h == nil {
		return ctxOr(ctx)
	}
	return context.WithValue(ctxOr(ctx), hostCtxKey{}, h)
}

// From 取回 ctx 里的 Host。**永不返回 nil**：没有时返回一个拒绝一切的替身，
// 并（只）打一条 warn——静默返回零接口会让插件在第一次调用时 panic。
func From(ctx context.Context) Host {
	if ctx != nil {
		if h, ok := ctx.Value(hostCtxKey{}).(Host); ok && h != nil {
			return h
		}
	}
	missingHostOnce.Do(func() {
		slog.Default().Warn("host.From：ctx 里没有 Bot 的 Host，使用拒绝性替身",
			"hint", "只有 manifest / close 这类进程级路径可以没有 Host；进程内请用 Init 拿到的 Host 走带 ctx 的调用")
	})
	return missingHost{}
}

// WithBot 标注「这次调用属于哪个 Bot」。核心侧唯一用途是让共享的插件句柄
// 按 Bot 分发（一个进程服务 N 个 Bot，而 Init / Handle / RunJob 的签名里
// 没有 bot 参数）。
func WithBot(ctx context.Context, b BotInfo) context.Context {
	return context.WithValue(ctxOr(ctx), botCtxKey{}, b)
}

// BotOf 读取 WithBot 标进去的 Bot。第二个返回值为 false 表示调用方没标注
// （零值的 BotInfo 也是零值，但"没标注"与"标注了空串"要能区分）。
func BotOf(ctx context.Context) (BotInfo, bool) {
	if ctx == nil {
		return BotInfo{}, false
	}
	b, ok := ctx.Value(botCtxKey{}).(BotInfo)
	return b, ok
}

func ctxOr(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// missingHostOnce 保证替身的 warn 只打一次：每个进程级调用都刷一条会把日志
// 淹掉，而这条信息只在"第一次用错上下文"时有用。
var missingHostOnce sync.Once

// errNoHost 是替身统一的拒绝原因。它是**普通错误**而不是哨兵：插件不需要
// 分支处理它——走到这里说明调用姿势本身错了，改代码才是解法。
var errNoHost = errors.New("host: 当前调用没有 Bot 的 Host（ctx 里没有 Bot 上下文）")

// missingHost 是 From 的兜底实现：所有能力调用都失败，Bot() 返回零值。
//
// 它存在的意义只有一个：让"没拿 ctx 就跨 Bot 读"从 panic 或串库，变成一条
// 一眼能读懂的错误。它不做任何兜底，也不假装自己是某个 Bot。
type missingHost struct{}

func (missingHost) Plugin() string                                  { return "" }
func (missingHost) Config() Params                                  { return Params{} }
func (missingHost) Bot() BotInfo                                    { return BotInfo{} }
func (missingHost) Log() *slog.Logger                               { return slog.Default() }
func (missingHost) Renderer() Renderer                              { return resolveRenderer(nil) }
func (missingHost) Session() Session                                { return resolveSession(nil, "") }
func (missingHost) Sender() Sender                                  { return missingSender{} }
func (missingHost) Scheduler() Scheduler                            { return missingSched{} }
func (missingHost) Admins() AdminDirectory                          { return missingAdmins{} }
func (missingHost) KV() KV                                          { return missingKV{} }
func (missingHost) PeerKV(string) KV                                { return missingKV{} }
func (missingHost) Query() QueryStore                               { return missingQuery{} }
func (missingHost) HTTP() HTTP                                      { return missingHTTP{} }
func (missingHost) IsAdmin(context.Context, Identity) (bool, error) { return false, errNoHost }

type missingSender struct{}

func (missingSender) Reply(context.Context, *Event, Reply) error { return errNoHost }
func (missingSender) SendActive(context.Context, Scene, string, Reply) error {
	return errNoHost
}

type missingSched struct{}

func (missingSched) Register(JobSpec) error { return errNoHost }

type missingAdmins struct{}

func (missingAdmins) Targets(context.Context) ([]AdminTarget, error) { return nil, errNoHost }

type missingKV struct{}

func (missingKV) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, errNoHost
}
func (missingKV) Set(context.Context, string, []byte, time.Duration) error { return errNoHost }
func (missingKV) Delete(context.Context, string) error                     { return errNoHost }

type missingQuery struct{}

func (missingQuery) Query(context.Context, string, ...any) ([]Row, error) { return nil, errNoHost }

type missingHTTP struct{}

func (missingHTTP) GetJSON(context.Context, string, any) error { return errNoHost }
func (missingHTTP) PostJSON(context.Context, string, any, any) error {
	return errNoHost
}
func (missingHTTP) GetJSONWithHeader(context.Context, string, map[string]string, any) error {
	return errNoHost
}
func (missingHTTP) GetJSONOnce(context.Context, string, map[string]string, any) error {
	return errNoHost
}
func (m missingHTTP) WithTimeout(time.Duration) HTTP { return m }

// 编译期确认替身把 Host 与它的全部子接口都实现了：少一个方法，插件拿到的是
// 一个缺方法的接口，症状是运行期 nil 调用。
var (
	_ Host           = missingHost{}
	_ Sender         = missingSender{}
	_ Scheduler      = missingSched{}
	_ AdminDirectory = missingAdmins{}
	_ KV             = missingKV{}
	_ QueryStore     = missingQuery{}
	_ HTTP           = missingHTTP{}
)

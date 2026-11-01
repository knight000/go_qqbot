package rpc

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// HostClient 用一条连接实现 host.Host——插件侧看到的就是它。
//
// 它与 ServeHost 是同一个协议的两半：那边把 host.* 折成帧，这边把帧还原成
// 方法调用。所有能力都只是转调，**不含任何判定**：域名白名单、权限、日预算、
// 会话冲突全在核心侧，插件绕过客户端也绕不过核心（§3.3 的隔离靠位置，
// 不靠插件自觉）。
//
// 一个 HostClient 绑定一个 Bot（Init 时由核心告知）。共享插件进程之后同一个
// 插件实例会有多个绑定，所以每个方法在执行时都先问 ctx：`host.From(ctx)`
// 若是另一个绑定的 HostClient，就按它的 Bot 发这条 RPC（ADR-0008 决策 11）。
// 于是插件把 Init 拿到的 h 存进结构体、在 Handle 里 `h.KV().Set(ctx, …)`，
// 写进的是**当前事件所属 Bot** 的库——插件无感，而且并发安全：这里没有任何
// "当前 Bot"的可变字段。
//
// 三个访问器是本地值（Plugin / Config / Log），理由见 method.go 的说明。
// Config 是例外中的例外：它没有 ctx，所以永远返回**本绑定**的配置；要读别的
// Bot 的配置就用 `host.From(ctx).Config()`。
type HostClient struct {
	conn *Conn
	name string
	cfg  host.Params
	bot  host.BotInfo
	log  *slog.Logger
	// fallbackOnce 让「这次调用没有 Bot 上下文」的告警每份客户端只打一次。
	// 共享进程下每个事件都可能走到那条路，逐次打会把 journald 淹掉。
	fallbackOnce sync.Once
}

var _ host.Host = (*HostClient)(nil)

// NewHostClient 构造插件侧宿主视图。name 与 cfg 来自核心的握手（plugin.init），
// bot 是这次握手所属的 Bot（plugin.init 的 bot_id + cert_type）。
func NewHostClient(c *Conn, name string, cfg map[string]any, bot host.BotInfo) *HostClient {
	if cfg == nil {
		cfg = map[string]any{}
	}
	logger := slog.Default()
	if name != "" {
		logger = logger.With("plugin", name)
	}
	if !bot.IsZero() {
		logger = logger.With("bot", bot.AppID)
	}
	return &HostClient{conn: c, name: name, cfg: host.NewParams(name, cfg), bot: bot, log: logger}
}

func (h *HostClient) Plugin() string      { return h.name }
func (h *HostClient) Config() host.Params { return h.cfg }
func (h *HostClient) Log() *slog.Logger   { return h.log }
func (h *HostClient) Bot() host.BotInfo   { return h.bot }

// botFor 解析「这次调用属于哪个 Bot」，三级优先级：
//
//  1. ctx 里显式标注的 Bot（`host.WithBot(ctx, h.Bot())`）。**插件自己的后台
//     协程用它**：goroutine 里没有「当前事件」，只有它捕获到的那份 h，把 Bot
//     钉进 ctx 才不会落到别人的库上。显式钉住的比"环境"优先——写下来的是意图。
//  2. ctx 里那份 HostClient——核心在投递 handle / start / run_job 时放进去的
//     「当前 Bot」。这是事件处理里唯一正确的来源，也是插件把 Init 的 h 存进
//     结构体却依然不会串库的原因。
//  3. 本绑定的 Bot（Init 时核心告知的那个）。
//
// 顺序把 1 放在 2 前面还有一层原因：插件侧 ctx 里只有 Host（BotOf 是核心侧
// 跨进程之前用的），所以两者本来就不会撞；而先看 BotOf 能保证"显式钉了 Bot"
// 的调用不再去摸 host.From——那条路会在 ctx 缺 Host 时打一条与本次调用无关的
// 「返回拒绝性替身」告警。
//
// 第三级不能删——`Scheduler().Register` 的签名里没有 ctx（契约如此），它只能
// 用本绑定；插件在 Init 里为某个 Bot 注册任务正是靠这个语义。但它也是唯一
// 「可能不是你以为的那个 Bot」的路径：把一个 Init 拿到的 h 存进结构体、又在
// 另一个 Bot 的事件里发无 ctx 的调用，落点就是上一次 Init 的 Bot。所以走到
// 第三级时记一条 warn（每份客户端一次），把静默串库变成日志里看得见的一行。
//
// 只认 *HostClient：ctx 里若是别的 Host 实现（进程内插件、测试替身），本方法
// 没有它的线协议可用，同样落到第三级。
func (h *HostClient) botFor(ctx context.Context) host.BotInfo {
	if b, ok := host.BotOf(ctx); ok && !b.IsZero() {
		return b
	}
	if o, ok := host.From(ctx).(*HostClient); ok && !o.bot.IsZero() {
		return o.bot
	}
	h.noteFallback()
	return h.bot
}

// noteFallback 把「用了第三级」这件事记下来。
//
// 为什么是 warn 而不是错误：错误会打断一条本来就合法的调用（Init 里注册任务、
// 单 Bot 部署下的随手调用），而这里要解决的是「静默」——同一条 warn 里给出正确
// 写法，插件作者改一次就再也不会看到它。
func (h *HostClient) noteFallback() {
	h.fallbackOnce.Do(func() {
		if h.bot.IsZero() {
			h.log.Warn("这次调用没有 Bot 上下文，本客户端也没绑定 Bot：host.* 会被核心按「缺少 bot_id」拒绝",
				"hint", "用 Init 收到的 Host 发起带 ctx 的调用，或用 host.WithBot(ctx, h.Bot()) 钉住 Bot")
			return
		}
		h.log.Warn("这次调用没有 Bot 上下文，回落到 Init 时的绑定",
			"bot", h.bot.AppID,
			"hint", "后台协程请用 host.WithBot(ctx, h.Bot()) 钉住 Bot；把某个 Bot 的 Init Host 存进结构体、再到别的 Bot 的事件里发无 ctx 调用，落点就是那个 Bot")
	})
}

func (h *HostClient) Sender() host.Sender         { return senderClient{h} }
func (h *HostClient) KV() host.KV                 { return kvClient{h, ""} }
func (h *HostClient) PeerKV(peer string) host.KV  { return kvClient{h, peer} }
func (h *HostClient) Query() host.QueryStore      { return queryClient{h} }
func (h *HostClient) HTTP() host.HTTP             { return httpClient{h, 0} }
func (h *HostClient) Scheduler() host.Scheduler   { return schedClient{h} }
func (h *HostClient) Admins() host.AdminDirectory { return adminDirClient{h} }
func (h *HostClient) Renderer() host.Renderer     { return rendererClient{h} }
func (h *HostClient) Session() host.Session       { return sessionClient{h} }

// ctxOr 兜住 nil ctx：插件的回调里传 nil 是常事，而 conn.Call 会把 nil
// 直接交给 select，那是 panic 而不是超时。
func ctxOr(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (h *HostClient) IsAdmin(ctx context.Context, id host.Identity) (bool, error) {
	res, err := h.conn.Call(ctxOr(ctx), MethodHostIsAdmin, isAdminParams{
		Bot: h.botFor(ctx).AppID, ID: toIdentityDTO(id),
	})
	if err != nil {
		return false, err
	}
	var out isAdminResult
	if err := res.Decode(&out); err != nil {
		return false, err
	}
	return out.Admin, nil
}

// —— Sender ——

type senderClient struct{ h *HostClient }

func (s senderClient) Reply(ctx context.Context, ev *host.Event, msg host.Reply) error {
	dto, body := encodeReply(&msg)
	_, err := s.h.conn.CallBody(ctxOr(ctx), MethodHostReply, replyParams{
		Bot: s.h.botFor(ctx).AppID, Event: toEventDTO(ev), Msg: dto,
	}, body)
	return err
}

func (s senderClient) SendActive(ctx context.Context, scene host.Scene, peerID string, msg host.Reply) error {
	dto, body := encodeReply(&msg)
	_, err := s.h.conn.CallBody(ctxOr(ctx), MethodHostSendActive, sendActiveParams{
		Bot: s.h.botFor(ctx).AppID, Scene: string(scene), Peer: peerID, Msg: dto,
	}, body)
	return err
}

// —— KV / PeerKV ——

// kvClient 用 peer 字段区分两套命名空间：空串是插件级全局键，
// 非空是该会话的私有键（`/reset session` 只清后者）。
type kvClient struct {
	h    *HostClient
	peer string
}

func (k kvClient) Get(ctx context.Context, key string) ([]byte, bool, error) {
	res, err := k.h.conn.Call(ctxOr(ctx), MethodHostKVGet, kvParams{
		Bot: k.h.botFor(ctx).AppID, Peer: k.peer, Key: key,
	})
	if err != nil {
		return nil, false, err
	}
	var out kvGetResult
	if err := res.Decode(&out); err != nil {
		return nil, false, err
	}
	return out.Value, out.Found, nil
}

func (k kvClient) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	_, err := k.h.conn.Call(ctxOr(ctx), MethodHostKVSet, kvSetParams{
		Bot: k.h.botFor(ctx).AppID, Peer: k.peer, Key: key, Value: val, TTL: int64(ttl),
	})
	return err
}

func (k kvClient) Delete(ctx context.Context, key string) error {
	_, err := k.h.conn.Call(ctxOr(ctx), MethodHostKVDelete, kvParams{
		Bot: k.h.botFor(ctx).AppID, Peer: k.peer, Key: key,
	})
	return err
}

// —— Query ——

type queryClient struct{ h *HostClient }

func (q queryClient) Query(ctx context.Context, query string, args ...any) ([]host.Row, error) {
	res, err := q.h.conn.Call(ctxOr(ctx), MethodHostQuery, queryParams{
		Bot: q.h.botFor(ctx).AppID, Query: query, Args: args,
	})
	if err != nil {
		return nil, err
	}
	var out queryResult
	if err := res.Decode(&out); err != nil {
		return nil, err
	}
	// normalizeRows 对 nil 返回 nil，空行集（`[]`）仍是非 nil 空切片：
	// 「没有数据」与「查出零行」在插件侧是两种判断，别在这里抹平。
	return normalizeRows(out.Rows), nil
}

// —— HTTP ——

// httpClient 的时间是**视图状态**：WithTimeout 只改这一份副本，
// 每次调用把 timeout 带进参数，核心侧据此取它自己的 WithTimeout 视图
// （共享连接池与限流状态的那一份，决策 6）。
type httpClient struct {
	h       *HostClient
	timeout time.Duration
}

func (c httpClient) WithTimeout(d time.Duration) host.HTTP {
	return httpClient{h: c.h, timeout: d}
}

func (c httpClient) GetJSON(ctx context.Context, url string, out any) error {
	return c.call(ctx, MethodHostHTTPGet, c.params(ctx, url), out)
}

func (c httpClient) PostJSON(ctx context.Context, url string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	p := c.params(ctx, url)
	p.In = raw
	return c.call(ctx, MethodHostHTTPPost, p, out)
}

func (c httpClient) GetJSONWithHeader(ctx context.Context, url string, header map[string]string, out any) error {
	p := c.params(ctx, url)
	p.Header = header
	return c.call(ctx, MethodHostHTTPGetHeader, p, out)
}

func (c httpClient) GetJSONOnce(ctx context.Context, url string, header map[string]string, out any) error {
	p := c.params(ctx, url)
	p.Header = header
	return c.call(ctx, MethodHostHTTPGetOnce, p, out)
}

// params 把「谁 + 打哪 + 超时」这三件每次调用都要带的信息装好。四个方法
// 的公共部分各写一遍只会让漏填 Bot 的概率变成四倍。
func (c httpClient) params(ctx context.Context, url string) httpParams {
	return httpParams{Bot: c.h.botFor(ctx).AppID, URL: url, Timeout: int64(c.timeout)}
}

// call 把上游正文原样带回来再解进 out，与进程内的路径逐字一致
// （包括 out 为 nil 时「只关心出错与否」的用法）。
func (c httpClient) call(ctx context.Context, method string, p httpParams, out any) error {
	res, err := c.h.conn.Call(ctxOr(ctx), method, p)
	if err != nil {
		return err
	}
	var wrapper httpResult
	if err := res.Decode(&wrapper); err != nil {
		return err
	}
	if out == nil || len(wrapper.Out) == 0 {
		return nil
	}
	// 这里**刻意**用 encoding/json 的默认行为（数字变 float64）：
	// 进程内那条路上的 json.Unmarshal 就是这么做的。
	return json.Unmarshal(wrapper.Out, out)
}

// —— Scheduler ——

type schedClient struct{ h *HostClient }

// Register 没有 ctx（契约如此）。给个上限而不是 Background：核心卡住时
// 插件该在启动期就失败，而不是永远停在 Init 里。
//
// 没有 ctx 也就没有"当前 Bot"：这里用本绑定的 Bot——这正是 Init 里注册任务
// 的那个 Bot，语义正确。
func (s schedClient) Register(spec host.JobSpec) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := s.h.conn.Call(ctx, MethodHostSchedule, scheduleParams{
		Bot: s.h.bot.AppID, Spec: toJobSpecDTO(spec),
	})
	return err
}

// —— Admins ——

type adminDirClient struct{ h *HostClient }

func (a adminDirClient) Targets(ctx context.Context) ([]host.AdminTarget, error) {
	res, err := a.h.conn.Call(ctxOr(ctx), MethodHostAdminTargets, botParams{Bot: a.h.botFor(ctx).AppID})
	if err != nil {
		return nil, err
	}
	var out adminTargetsResult
	if err := res.Decode(&out); err != nil {
		return nil, err
	}
	if out.Targets == nil {
		return nil, nil
	}
	targets := make([]host.AdminTarget, len(out.Targets))
	for i, t := range out.Targets {
		targets[i] = t.toHost()
	}
	return targets, nil
}

// —— Renderer ——

type rendererClient struct{ h *HostClient }

// Markdown 的返回值可能带 1–3 MB 的 PNG，它走应答帧的正文槽
// （单帧往返、不 base64，见 frame.go）。
func (r rendererClient) Markdown(ctx context.Context, src string) (host.Image, error) {
	res, err := r.h.conn.Call(ctxOr(ctx), MethodHostRender, renderParams{
		Bot: r.h.botFor(ctx).AppID, Src: src,
	})
	if err != nil {
		return host.Image{}, err
	}
	var out renderResult
	if err := res.Decode(&out); err != nil {
		return host.Image{}, err
	}
	return decodeImage(out.Image, res.Body())
}

// —— Session ——

type sessionClient struct{ h *HostClient }

func (s sessionClient) Await(ctx context.Context, ev *host.Event, state []byte, ttl time.Duration) error {
	_, err := s.h.conn.Call(ctxOr(ctx), MethodHostSessionAwait, sessionAwaitParams{
		Bot: s.h.botFor(ctx).AppID, Event: toEventDTO(ev), State: state, TTL: int64(ttl),
	})
	return err
}

// 编译期确认：客户端把 host 的每个子接口都实现了，不然插件拿到的是
// 一个缺方法的结构体，症状是运行时 nil 接口调用。
var (
	_ host.Sender         = senderClient{}
	_ host.KV             = kvClient{}
	_ host.QueryStore     = queryClient{}
	_ host.HTTP           = httpClient{}
	_ host.Scheduler      = schedClient{}
	_ host.AdminDirectory = adminDirClient{}
	_ host.Renderer       = rendererClient{}
	_ host.Session        = sessionClient{}
)

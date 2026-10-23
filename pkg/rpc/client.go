package rpc

import (
	"context"
	"encoding/json"
	"log/slog"
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
// 三个访问器是本地值（Plugin / Config / Log），理由见 method.go 的说明。
type HostClient struct {
	conn *Conn
	name string
	cfg  host.Params
	log  *slog.Logger
}

var _ host.Host = (*HostClient)(nil)

// NewHostClient 构造插件侧宿主视图。name 与 cfg 来自核心的握手（plugin.init）。
func NewHostClient(c *Conn, name string, cfg map[string]any) *HostClient {
	if cfg == nil {
		cfg = map[string]any{}
	}
	logger := slog.Default()
	if name != "" {
		logger = logger.With("plugin", name)
	}
	return &HostClient{conn: c, name: name, cfg: host.NewParams(name, cfg), log: logger}
}

func (h *HostClient) Plugin() string      { return h.name }
func (h *HostClient) Config() host.Params { return h.cfg }
func (h *HostClient) Log() *slog.Logger   { return h.log }

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
	res, err := h.conn.Call(ctxOr(ctx), MethodHostIsAdmin, isAdminParams{ID: toIdentityDTO(id)})
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
		Event: toEventDTO(ev), Msg: dto,
	}, body)
	return err
}

func (s senderClient) SendActive(ctx context.Context, scene host.Scene, peerID string, msg host.Reply) error {
	dto, body := encodeReply(&msg)
	_, err := s.h.conn.CallBody(ctxOr(ctx), MethodHostSendActive, sendActiveParams{
		Scene: string(scene), Peer: peerID, Msg: dto,
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
	res, err := k.h.conn.Call(ctxOr(ctx), MethodHostKVGet, kvParams{Peer: k.peer, Key: key})
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
		Peer: k.peer, Key: key, Value: val, TTL: int64(ttl),
	})
	return err
}

func (k kvClient) Delete(ctx context.Context, key string) error {
	_, err := k.h.conn.Call(ctxOr(ctx), MethodHostKVDelete, kvParams{Peer: k.peer, Key: key})
	return err
}

// —— Query ——

type queryClient struct{ h *HostClient }

func (q queryClient) Query(ctx context.Context, query string, args ...any) ([]host.Row, error) {
	res, err := q.h.conn.Call(ctxOr(ctx), MethodHostQuery, queryParams{Query: query, Args: args})
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
	return c.call(ctx, MethodHostHTTPGet, httpParams{URL: url, Timeout: int64(c.timeout)}, out)
}

func (c httpClient) PostJSON(ctx context.Context, url string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.call(ctx, MethodHostHTTPPost, httpParams{URL: url, In: raw, Timeout: int64(c.timeout)}, out)
}

func (c httpClient) GetJSONWithHeader(ctx context.Context, url string, header map[string]string, out any) error {
	return c.call(ctx, MethodHostHTTPGetHeader, httpParams{URL: url, Header: header, Timeout: int64(c.timeout)}, out)
}

func (c httpClient) GetJSONOnce(ctx context.Context, url string, header map[string]string, out any) error {
	return c.call(ctx, MethodHostHTTPGetOnce, httpParams{URL: url, Header: header, Timeout: int64(c.timeout)}, out)
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
func (s schedClient) Register(spec host.JobSpec) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := s.h.conn.Call(ctx, MethodHostSchedule, scheduleParams{Spec: toJobSpecDTO(spec)})
	return err
}

// —— Admins ——

type adminDirClient struct{ h *HostClient }

func (a adminDirClient) Targets(ctx context.Context) ([]host.AdminTarget, error) {
	res, err := a.h.conn.Call(ctxOr(ctx), MethodHostAdminTargets, nil)
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
	res, err := r.h.conn.Call(ctxOr(ctx), MethodHostRender, renderParams{Src: src})
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
		Event: toEventDTO(ev), State: state, TTL: int64(ttl),
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

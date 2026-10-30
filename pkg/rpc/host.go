package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// HostResolver 按 bot_id 找「这个 Bot 在这个插件上的 Host」。
//
// 共享插件进程之后（ADR-0008 决策 11），一个插件进程服务 N 个 Bot，而
// host.* 的每一条调用都必须落到发出它的那个 Bot 上——插件侧无法区分，
// 区分只能发生在核心侧：由装配层为每个 (Bot, 插件) 注册一份 Host。
//
// 返回值是 error 而不是"找不到就给个空 Host"：串到别的 Bot 的库上比一次
// 明确的失败坏得多，而这类错误只会出现在装配缺陷里。
type HostResolver func(botID string) (host.Host, error)

// ServeHost 把 host.* 的全部方法注册到 c 上，由 resolve 按 bot_id 提供实现。
//
// 这是核心侧的**全部**跨进程工作量：路由、权限、调度、会话、渲染一行都不用
// 改，因为它们只认 host.Host 这个接口（ADR-0005 决策 3）。插件侧对称的
// 一半在 pkg/rpcsdk（它实现的是同一个接口的客户端版本）。
//
// 必须在 c.Run 之前调用。resolve 为 nil 时返回错误：一个「所有方法都在、但
// 每个都报未初始化」的连接比装配期直接失败难查得多。
func ServeHost(c *Conn, resolve HostResolver) error {
	if c == nil {
		return errors.New("rpc: ServeHost 需要非 nil 连接")
	}
	if resolve == nil {
		return errors.New("rpc: ServeHost 需要非 nil 的 Host 解析函数")
	}
	s := &hostServer{resolve: resolve}
	handlers := map[string]Handler{
		MethodHostReply:         s.reply,
		MethodHostSendActive:    s.sendActive,
		MethodHostKVGet:         s.kvGet,
		MethodHostKVSet:         s.kvSet,
		MethodHostKVDelete:      s.kvDelete,
		MethodHostQuery:         s.query,
		MethodHostHTTPGet:       s.httpGet,
		MethodHostHTTPPost:      s.httpPost,
		MethodHostHTTPGetHeader: s.httpGetHeader,
		MethodHostHTTPGetOnce:   s.httpGetOnce,
		MethodHostSchedule:      s.schedule,
		MethodHostIsAdmin:       s.isAdmin,
		MethodHostAdminTargets:  s.adminTargets,
		MethodHostRender:        s.render,
		MethodHostSessionAwait:  s.sessionAwait,
	}
	for method, handler := range handlers {
		if err := c.Handle(method, handler); err != nil {
			return err
		}
	}
	return nil
}

// hostServer 是 host.* 的实现。
//
// 它不做任何校验与兜底：校验在 host.Service 那一层（它已经把参数收敛成
// 可执行的值），这里重复一遍只会让「同一个错误有两种文案」。
type hostServer struct {
	resolve HostResolver
}

// forBot 解析本次调用该用哪一份 Host。
//
// Bot 为空必须是错误而不是"用默认 Bot"：进程级调用（manifest / close）不走
// host.*，能走到这里说明对端漏填 bot_id——挑一个 Bot 顶上就是跨租户串数据。
func (s *hostServer) forBot(method, botID string) (host.Host, error) {
	if botID == "" {
		return nil, fmt.Errorf("rpc: %s 缺少 bot_id（host.* 必须带 Bot）", method)
	}
	h, err := s.resolve(botID)
	if err != nil {
		return nil, fmt.Errorf("rpc: %s 找不到 Bot %s 的 Host: %w", method, botID, err)
	}
	if h == nil {
		return nil, fmt.Errorf("rpc: %s 找不到 Bot %s 的 Host", method, botID)
	}
	return h, nil
}

// badParams 说明是插件把参数写错了。归到 CodeUnknown 而不是新开一个码：
// 收侧对「参数错」没有不同的处置，而码是永久契约，能不加就不加。
func badParams(method string, err error) error {
	return fmt.Errorf("rpc: %s 的参数不合法: %w", method, err)
}

func (s *hostServer) reply(ctx context.Context, req Request) (Response, error) {
	var p replyParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostReply, err)
	}
	h, err := s.forBot(MethodHostReply, p.Bot)
	if err != nil {
		return Response{}, err
	}
	msg, err := decodeReply(p.Msg, req.Body)
	if err != nil {
		return Response{}, err
	}
	return Response{}, h.Sender().Reply(ctx, p.Event.toHost(), *msg)
}

func (s *hostServer) sendActive(ctx context.Context, req Request) (Response, error) {
	var p sendActiveParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostSendActive, err)
	}
	h, err := s.forBot(MethodHostSendActive, p.Bot)
	if err != nil {
		return Response{}, err
	}
	msg, err := decodeReply(p.Msg, req.Body)
	if err != nil {
		return Response{}, err
	}
	return Response{}, h.Sender().SendActive(ctx, host.Scene(p.Scene), p.Peer, *msg)
}

// kv 按 peer 是否为空选命名空间：插件侧的 KV() 与 PeerKV(peer) 是同三个
// 方法的两套视图，协议里用 peer 字段区分，不必开六个方法。
func kvOf(h host.Host, peer string) host.KV {
	if peer == "" {
		return h.KV()
	}
	return h.PeerKV(peer)
}

func (s *hostServer) kvGet(ctx context.Context, req Request) (Response, error) {
	var p kvParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostKVGet, err)
	}
	h, err := s.forBot(MethodHostKVGet, p.Bot)
	if err != nil {
		return Response{}, err
	}
	val, found, err := kvOf(h, p.Peer).Get(ctx, p.Key)
	if err != nil {
		return Response{}, err
	}
	return Response{Result: kvGetResult{Value: val, Found: found}}, nil
}

func (s *hostServer) kvSet(ctx context.Context, req Request) (Response, error) {
	var p kvSetParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostKVSet, err)
	}
	h, err := s.forBot(MethodHostKVSet, p.Bot)
	if err != nil {
		return Response{}, err
	}
	return Response{}, kvOf(h, p.Peer).Set(ctx, p.Key, p.Value, time.Duration(p.TTL))
}

func (s *hostServer) kvDelete(ctx context.Context, req Request) (Response, error) {
	var p kvParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostKVDelete, err)
	}
	h, err := s.forBot(MethodHostKVDelete, p.Bot)
	if err != nil {
		return Response{}, err
	}
	return Response{}, kvOf(h, p.Peer).Delete(ctx, p.Key)
}

func (s *hostServer) query(ctx context.Context, req Request) (Response, error) {
	var p queryParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostQuery, err)
	}
	h, err := s.forBot(MethodHostQuery, p.Bot)
	if err != nil {
		return Response{}, err
	}
	for i, a := range p.Args {
		p.Args[i] = normalizeNums(a)
	}
	rows, err := h.Query().Query(ctx, p.Query, p.Args...)
	if err != nil {
		return Response{}, err
	}
	return Response{Result: queryResult{Rows: rows}}, nil
}

// httpClientFor 返回本次调用要用的客户端。timeout > 0 时取 WithTimeout 视图
// （契约语义：共享连接池与限流状态，只替换整体超时）。
func httpClientFor(h host.Host, timeout int64) host.HTTP {
	hc := h.HTTP()
	if timeout > 0 {
		hc = hc.WithTimeout(time.Duration(timeout))
	}
	return hc
}

func (s *hostServer) httpGet(ctx context.Context, req Request) (Response, error) {
	var p httpParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostHTTPGet, err)
	}
	h, err := s.forBot(MethodHostHTTPGet, p.Bot)
	if err != nil {
		return Response{}, err
	}
	var out json.RawMessage
	if err := httpClientFor(h, p.Timeout).GetJSON(ctx, p.URL, &out); err != nil {
		return Response{}, err
	}
	return Response{Result: httpResult{Out: out}}, nil
}

func (s *hostServer) httpPost(ctx context.Context, req Request) (Response, error) {
	var p httpParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostHTTPPost, err)
	}
	h, err := s.forBot(MethodHostHTTPPost, p.Bot)
	if err != nil {
		return Response{}, err
	}
	var out json.RawMessage
	if err := httpClientFor(h, p.Timeout).PostJSON(ctx, p.URL, p.In, &out); err != nil {
		return Response{}, err
	}
	return Response{Result: httpResult{Out: out}}, nil
}

func (s *hostServer) httpGetHeader(ctx context.Context, req Request) (Response, error) {
	var p httpParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostHTTPGetHeader, err)
	}
	h, err := s.forBot(MethodHostHTTPGetHeader, p.Bot)
	if err != nil {
		return Response{}, err
	}
	var out json.RawMessage
	if err := httpClientFor(h, p.Timeout).GetJSONWithHeader(ctx, p.URL, p.Header, &out); err != nil {
		return Response{}, err
	}
	return Response{Result: httpResult{Out: out}}, nil
}

func (s *hostServer) httpGetOnce(ctx context.Context, req Request) (Response, error) {
	var p httpParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostHTTPGetOnce, err)
	}
	h, err := s.forBot(MethodHostHTTPGetOnce, p.Bot)
	if err != nil {
		return Response{}, err
	}
	var out json.RawMessage
	if err := httpClientFor(h, p.Timeout).GetJSONOnce(ctx, p.URL, p.Header, &out); err != nil {
		return Response{}, err
	}
	return Response{Result: httpResult{Out: out}}, nil
}

func (s *hostServer) schedule(ctx context.Context, req Request) (Response, error) {
	var p scheduleParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostSchedule, err)
	}
	h, err := s.forBot(MethodHostSchedule, p.Bot)
	if err != nil {
		return Response{}, err
	}
	// 不信任 spec.Plugin：host.Service 的 pluginSched 会用真实插件名覆盖它，
	// 但那是实现细节；这里也不替它做——两处都改才是把「插件只能注册自己
	// 的任务」这条约束写死。
	return Response{}, h.Scheduler().Register(p.Spec.toHost())
}

func (s *hostServer) isAdmin(ctx context.Context, req Request) (Response, error) {
	var p isAdminParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostIsAdmin, err)
	}
	h, err := s.forBot(MethodHostIsAdmin, p.Bot)
	if err != nil {
		return Response{}, err
	}
	ok, err := h.IsAdmin(ctx, p.ID.toHost())
	if err != nil {
		return Response{}, err
	}
	return Response{Result: isAdminResult{Admin: ok}}, nil
}

func (s *hostServer) adminTargets(ctx context.Context, req Request) (Response, error) {
	var p botParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostAdminTargets, err)
	}
	h, err := s.forBot(MethodHostAdminTargets, p.Bot)
	if err != nil {
		return Response{}, err
	}
	targets, err := h.Admins().Targets(ctx)
	if err != nil {
		return Response{}, err
	}
	return Response{Result: adminTargetsResult{Targets: toAdminTargetDTOs(targets)}}, nil
}

func (s *hostServer) render(ctx context.Context, req Request) (Response, error) {
	var p renderParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostRender, err)
	}
	h, err := s.forBot(MethodHostRender, p.Bot)
	if err != nil {
		return Response{}, err
	}
	img, err := h.Renderer().Markdown(ctx, p.Src)
	if err != nil {
		return Response{}, err
	}
	// 1–3 MB 的 PNG 走正文槽：单帧往返、不 base64（ADR-0005 决策 4）。
	dto, body := encodeImage(img)
	return Response{Result: renderResult{Image: dto}, Body: body}, nil
}

func (s *hostServer) sessionAwait(ctx context.Context, req Request) (Response, error) {
	var p sessionAwaitParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostSessionAwait, err)
	}
	h, err := s.forBot(MethodHostSessionAwait, p.Bot)
	if err != nil {
		return Response{}, err
	}
	return Response{}, h.Session().Await(ctx, p.Event.toHost(), p.State, time.Duration(p.TTL))
}

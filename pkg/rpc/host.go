package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// ServeHost 把 host.* 的全部方法注册到 c 上，由 h 提供实现。
//
// 这是核心侧的**全部**跨进程工作量：路由、权限、调度、会话、渲染一行都不用
// 改，因为它们只认 host.Host 这个接口（ADR-0005 决策 3）。插件侧对称的
// 一半在 pkg/rpcsdk（它实现的是同一个接口的客户端版本）。
//
// 必须在 c.Run 之前调用。h 为 nil 时返回错误：一个「所有方法都在、但每个都
// 报未初始化」的连接比装配期直接失败难查得多。
func ServeHost(c *Conn, h host.Host) error {
	if c == nil {
		return errors.New("rpc: ServeHost 需要非 nil 连接")
	}
	if h == nil {
		return errors.New("rpc: ServeHost 需要非 nil Host")
	}
	s := &hostServer{h: h}
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
	h host.Host
}

// badParams 说明是插件把参数写错了。归到 CodeUnknown 而不是新开一个码：
// 收侧对「参数错」没有不同的处置，而码是永久契约，能不加就不加。
func badParams(method string, err error) error {
	return fmt.Errorf("rpc: %s 的参数不合法: %w", method, err)
}

func (s *hostServer) reply(ctx context.Context, req Request) (Response, error) {
	var p struct {
		Event eventDTO
		Msg   replyDTO
	}
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostReply, err)
	}
	msg, err := decodeReply(p.Msg, req.Body)
	if err != nil {
		return Response{}, err
	}
	return Response{}, s.h.Sender().Reply(ctx, p.Event.toHost(), *msg)
}

func (s *hostServer) sendActive(ctx context.Context, req Request) (Response, error) {
	var p struct {
		Scene string
		Peer  string
		Msg   replyDTO
	}
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostSendActive, err)
	}
	msg, err := decodeReply(p.Msg, req.Body)
	if err != nil {
		return Response{}, err
	}
	return Response{}, s.h.Sender().SendActive(ctx, host.Scene(p.Scene), p.Peer, *msg)
}

// kv 按 peer 是否为空选命名空间：插件侧的 KV() 与 PeerKV(peer) 是同三个
// 方法的两套视图，协议里用 peer 字段区分，不必开六个方法。
func (s *hostServer) kv(peer string) host.KV {
	if peer == "" {
		return s.h.KV()
	}
	return s.h.PeerKV(peer)
}

func (s *hostServer) kvGet(ctx context.Context, req Request) (Response, error) {
	var p struct{ Peer, Key string }
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostKVGet, err)
	}
	val, found, err := s.kv(p.Peer).Get(ctx, p.Key)
	if err != nil {
		return Response{}, err
	}
	return Response{Result: struct {
		Value []byte
		Found bool
	}{val, found}}, nil
}

func (s *hostServer) kvSet(ctx context.Context, req Request) (Response, error) {
	var p struct {
		Peer  string
		Key   string
		Value []byte
		TTL   int64
	}
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostKVSet, err)
	}
	return Response{}, s.kv(p.Peer).Set(ctx, p.Key, p.Value, time.Duration(p.TTL))
}

func (s *hostServer) kvDelete(ctx context.Context, req Request) (Response, error) {
	var p struct{ Peer, Key string }
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostKVDelete, err)
	}
	return Response{}, s.kv(p.Peer).Delete(ctx, p.Key)
}

func (s *hostServer) query(ctx context.Context, req Request) (Response, error) {
	var p struct {
		Query string
		Args  []any
	}
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostQuery, err)
	}
	for i, a := range p.Args {
		p.Args[i] = normalizeNums(a)
	}
	rows, err := s.h.Query().Query(ctx, p.Query, p.Args...)
	if err != nil {
		return Response{}, err
	}
	return Response{Result: struct{ Rows []host.Row }{rows}}, nil
}

// httpResult 把上游 JSON **原样**带回去，不在核心侧解一遍再编一遍。
//
// 解一遍的代价是保真：解进 any 会把所有数字变成 float64（int64 里超过 2^53
// 的会被改掉），再编回去就是另一份数据了。收侧按自己的 out 类型解，
// 与进程内那条路逐字一致。
type httpResult struct {
	Out json.RawMessage
}

// httpParams 是四个 HTTP 方法的公共入参（各自的字段互有冗余，但一个结构体
// 比四个只在两三个字段上有差别的结构体好维护）。
type httpParams struct {
	URL     string
	In      json.RawMessage
	Header  map[string]string
	Timeout int64 // 纳秒；0 表示用默认超时
}

// http 返回本次调用要用的客户端。timeout > 0 时取 WithTimeout 视图
// （契约语义：共享连接池与限流状态，只替换整体超时）。
func (s *hostServer) http(timeout int64) host.HTTP {
	hc := s.h.HTTP()
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
	var out json.RawMessage
	if err := s.http(p.Timeout).GetJSON(ctx, p.URL, &out); err != nil {
		return Response{}, err
	}
	return Response{Result: httpResult{Out: out}}, nil
}

func (s *hostServer) httpPost(ctx context.Context, req Request) (Response, error) {
	var p httpParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostHTTPPost, err)
	}
	var out json.RawMessage
	if err := s.http(p.Timeout).PostJSON(ctx, p.URL, p.In, &out); err != nil {
		return Response{}, err
	}
	return Response{Result: httpResult{Out: out}}, nil
}

func (s *hostServer) httpGetHeader(ctx context.Context, req Request) (Response, error) {
	var p httpParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostHTTPGetHeader, err)
	}
	var out json.RawMessage
	if err := s.http(p.Timeout).GetJSONWithHeader(ctx, p.URL, p.Header, &out); err != nil {
		return Response{}, err
	}
	return Response{Result: httpResult{Out: out}}, nil
}

func (s *hostServer) httpGetOnce(ctx context.Context, req Request) (Response, error) {
	var p httpParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostHTTPGetOnce, err)
	}
	var out json.RawMessage
	if err := s.http(p.Timeout).GetJSONOnce(ctx, p.URL, p.Header, &out); err != nil {
		return Response{}, err
	}
	return Response{Result: httpResult{Out: out}}, nil
}

func (s *hostServer) schedule(ctx context.Context, req Request) (Response, error) {
	var p struct{ Spec jobSpecDTO }
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostSchedule, err)
	}
	// 不信任 spec.Plugin：host.Service 的 pluginSched 会用真实插件名覆盖它，
	// 但那是实现细节；这里也不替它做——两处都改才是把「插件只能注册自己
	// 的任务」这条约束写死。
	return Response{}, s.h.Scheduler().Register(p.Spec.toHost())
}

func (s *hostServer) isAdmin(ctx context.Context, req Request) (Response, error) {
	var p struct{ ID identityDTO }
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostIsAdmin, err)
	}
	ok, err := s.h.IsAdmin(ctx, p.ID.toHost())
	if err != nil {
		return Response{}, err
	}
	return Response{Result: struct{ Admin bool }{ok}}, nil
}

func (s *hostServer) adminTargets(ctx context.Context, _ Request) (Response, error) {
	targets, err := s.h.Admins().Targets(ctx)
	if err != nil {
		return Response{}, err
	}
	return Response{Result: struct{ Targets []adminTargetDTO }{toAdminTargetDTOs(targets)}}, nil
}

func (s *hostServer) render(ctx context.Context, req Request) (Response, error) {
	var p struct{ Src string }
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostRender, err)
	}
	img, err := s.h.Renderer().Markdown(ctx, p.Src)
	if err != nil {
		return Response{}, err
	}
	// 1–3 MB 的 PNG 走正文槽：单帧往返、不 base64（ADR-0005 决策 4）。
	dto, body := encodeImage(img)
	return Response{Result: struct{ Image imageDTO }{dto}, Body: body}, nil
}

func (s *hostServer) sessionAwait(ctx context.Context, req Request) (Response, error) {
	var p struct {
		Event eventDTO
		State []byte
		TTL   int64
	}
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodHostSessionAwait, err)
	}
	return Response{}, s.h.Session().Await(ctx, p.Event.toHost(), p.State, time.Duration(p.TTL))
}

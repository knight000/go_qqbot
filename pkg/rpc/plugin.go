package rpc

import (
	"context"
	"errors"
	"sync"

	"github.com/knight000/go_qqbot/pkg/host"
)

// ServePlugin 把 plugin.* 的方法注册到 c 上，由 p 提供实现。
//
// 它是 ServeHost 的镜像：核心那边服务 host.*、调用 plugin.*，插件这边反过来。
// 生命周期以 `plugin.init` 为分界线：除 manifest 之外的方法都等这次调用结束
// （成功或失败）才放行——否则一条早到的 handle 会读到还没写好的 Config，
// 表现为「偶尔读不到配置」，而那是最难查的一类故障。
//
// Init 由**核心**驱动（决策 5：起子进程 → 发 init（带配置）→ 进入可路由状态），
// 不是插件自己在 main 里调：配置快照必须由核心给，插件自己也拿不到。
//
// p 为 nil 或 Manifest.Name 为空时返回错误：这类装配错误必须在装配期暴露。
func ServePlugin(c *Conn, p host.Plugin) error {
	if c == nil {
		return errors.New("rpc: ServePlugin 需要非 nil 连接")
	}
	if p == nil {
		return errors.New("rpc: ServePlugin 需要非 nil 插件")
	}
	if p.Manifest().Name == "" {
		return errors.New("rpc: 插件 Manifest.Name 不能为空")
	}

	s := &pluginServer{c: c, p: p, gate: make(chan struct{})}
	// 可选接口用类型断言决定要不要注册方法：跨进程没有类型可断言，
	// 所以「实现了哪些」由 plugin.manifest 的 Optional 一并报给核心。
	if v, ok := p.(host.Starter); ok {
		s.starter = v
	}
	if v, ok := p.(host.Closer); ok {
		s.closer = v
	}
	if v, ok := p.(host.JobRunner); ok {
		s.jobber = v
	}

	methods := map[string]Handler{
		MethodPluginManifest: s.manifest,
		MethodPluginInit:     s.init,
		MethodPluginHandle:   s.handle,
	}
	if s.starter != nil {
		methods[MethodPluginStart] = s.start
	}
	if s.closer != nil {
		methods[MethodPluginClose] = s.close
	}
	if s.jobber != nil {
		methods[MethodPluginRunJob] = s.runJob
	}
	for method, handler := range methods {
		if err := c.Handle(method, handler); err != nil {
			return err
		}
	}
	return nil
}

type pluginServer struct {
	c    *Conn
	p    host.Plugin
	gate chan struct{}

	starter host.Starter
	closer  host.Closer
	jobber  host.JobRunner

	mu      sync.Mutex
	inited  bool
	initErr error
}

// optional 是「本产物实现了哪几个可选接口」的清单，随 manifest 一起报给核心。
func (s *pluginServer) optional() []string {
	var out []string
	if s.starter != nil {
		out = append(out, OptionalStart)
	}
	if s.closer != nil {
		out = append(out, OptionalClose)
	}
	if s.jobber != nil {
		out = append(out, OptionalRunJob)
	}
	return out
}

func (s *pluginServer) manifest(_ context.Context, _ Request) (Response, error) {
	return Response{Result: struct {
		Manifest manifestDTO
		Optional []string
	}{toManifestDTO(s.p.Manifest()), s.optional()}}, nil
}

func (s *pluginServer) init(ctx context.Context, req Request) (Response, error) {
	var p struct {
		Name   string
		Config map[string]any
	}
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodPluginInit, err)
	}
	s.mu.Lock()
	if s.inited {
		s.mu.Unlock()
		return Response{}, errors.New("rpc: 插件已经初始化过（Init 只调用一次）")
	}
	s.inited = true
	s.mu.Unlock()

	name := p.Name
	if name == "" {
		name = s.p.Manifest().Name
	}
	vals := p.Config
	if vals != nil {
		// TOML 的整数在 JSON 里会变成 float64/json.Number，必须先收回 int64，
		// 否则 Params.Int 拿得到值、Params.Decode 解进 int 字段会报错。
		vals = normalizeNums(vals).(map[string]any)
	}
	err := s.p.Init(ctx, NewHostClient(s.c, name, vals))

	s.mu.Lock()
	s.initErr = err
	s.mu.Unlock()
	// 失败也要放行：Close 得能跑（清理半初始化的资源），而其他方法会拿到
	// initErr 而不是挂在这里等一个永远不会来的成功。
	close(s.gate)
	return Response{}, err
}

// wait 等 Init 结束。ctx 到期时如实返回 ctx 的错误——插件卡在 Init 里时，
// 核心看到的应该是「超时」，而不是「方法未注册」。
func (s *pluginServer) wait(ctx context.Context) error {
	select {
	case <-s.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initErr
}

func (s *pluginServer) handle(ctx context.Context, req Request) (Response, error) {
	if err := s.wait(ctx); err != nil {
		return Response{}, err
	}
	var p struct{ Event eventDTO }
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodPluginHandle, err)
	}
	reply, err := s.p.Handle(ctx, p.Event.toHost())
	if err != nil {
		return Response{}, err
	}
	// nil 与空 Reply 必须区分：进程内 nil 表示「这条不回复」，而空 Reply
	// 会让核心去发一条空消息。所以结果段用指针，nil 原样传过去。
	if reply == nil {
		return Response{Result: struct{ Reply *replyDTO }{}}, nil
	}
	dto, body := encodeReply(reply)
	return Response{Result: struct{ Reply *replyDTO }{&dto}, Body: body}, nil
}

func (s *pluginServer) start(ctx context.Context, _ Request) (Response, error) {
	if err := s.wait(ctx); err != nil {
		return Response{}, err
	}
	return Response{}, s.starter.Start(ctx)
}

func (s *pluginServer) close(ctx context.Context, _ Request) (Response, error) {
	// Close 不看 initErr：半初始化同样要收摊。
	select {
	case <-s.gate:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	return Response{}, s.closer.Close(ctx)
}

func (s *pluginServer) runJob(ctx context.Context, req Request) (Response, error) {
	if err := s.wait(ctx); err != nil {
		return Response{}, err
	}
	var p struct{ Spec jobSpecDTO }
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodPluginRunJob, err)
	}
	return Response{}, s.jobber.RunJob(ctx, p.Spec.toHost())
}

// —— 调用侧：核心怎么驱动一个插件 ——
//
// 这六个方法把 plugin.* 的线格式包成 host 类型，与 HostClient 对称。
// 它们刻意留在这里而不是 core 里：线格式的知识只应存在于一个包，
// 否则 #40 的 proxy 会再写一遍 DTO 映射，两处漂移就是两套协议。

// CallManifest 拉取清单与可选方法表。这是握手第一步。
//
// 返回的第二个值是插件**真的**实现了哪几个可选接口（plugin.start /
// plugin.close / plugin.run_job）。跨进程没有类型可断言，核心据此决定要不要
// 发 start，而不是靠「先调一次，失败就当没有」——后者会把真正的故障
// （插件启动即崩）和「本来就没实现」混成同一个现象。
func (c *Conn) CallManifest(ctx context.Context) (host.Manifest, []string, error) {
	res, err := c.Call(ctx, MethodPluginManifest, nil)
	if err != nil {
		return host.Manifest{}, nil, err
	}
	var out struct {
		Manifest manifestDTO
		Optional []string
	}
	if err := res.Decode(&out); err != nil {
		return host.Manifest{}, nil, err
	}
	return out.Manifest.toHost(), out.Optional, nil
}

// CallInit 把配置快照交给插件并让它初始化。cfg 是 [plugins.<name>] 段的原始 map。
func (c *Conn) CallInit(ctx context.Context, name string, cfg map[string]any) error {
	_, err := c.Call(ctx, MethodPluginInit, struct {
		Name   string
		Config map[string]any
	}{name, cfg})
	return err
}

// CallHandle 投递一个事件。返回 nil 表示插件选择不回复（进程内同样的语义）：
// 它与「回复一条空消息」是两件事，所以这里保留 nil。
func (c *Conn) CallHandle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	res, err := c.Call(ctx, MethodPluginHandle, struct{ Event eventDTO }{toEventDTO(ev)})
	if err != nil {
		return nil, err
	}
	var out struct{ Reply *replyDTO }
	if err := res.Decode(&out); err != nil {
		return nil, err
	}
	if out.Reply == nil {
		return nil, nil
	}
	return decodeReply(*out.Reply, res.Body())
}

// CallStart 调 Start。插件没实现时返回的 err 命中 ErrMethodNotFound。
func (c *Conn) CallStart(ctx context.Context) error {
	_, err := c.Call(ctx, MethodPluginStart, nil)
	return err
}

// CallClose 调 Close，用于关停与重启前的收尾。
func (c *Conn) CallClose(ctx context.Context) error {
	_, err := c.Call(ctx, MethodPluginClose, nil)
	return err
}

// CallRunJob 执行一个到点的任务。超时由调用方按 JobSpec.Timeout 设在 ctx 上。
func (c *Conn) CallRunJob(ctx context.Context, spec host.JobSpec) error {
	_, err := c.Call(ctx, MethodPluginRunJob, struct{ Spec jobSpecDTO }{toJobSpecDTO(spec)})
	return err
}

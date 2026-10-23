package rpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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

	s := &pluginServer{c: c, p: p, gate: make(chan struct{}), caps: capabilitiesOf(p)}

	methods := map[string]Handler{
		MethodPluginManifest: s.manifest,
		MethodPluginInit:     s.init,
		MethodPluginHandle:   s.handle,
	}
	if s.caps.starter != nil {
		methods[MethodPluginStart] = s.start
	}
	if s.caps.closer != nil {
		methods[MethodPluginClose] = s.close
	}
	if s.caps.jobber != nil {
		methods[MethodPluginRunJob] = s.runJob
	}
	for method, handler := range methods {
		if err := c.Handle(method, handler); err != nil {
			return err
		}
	}
	return nil
}

// pluginCaps 是「本产物实现了哪几个可选接口」的唯一事实源。
//
// 注册哪几个方法、plugin.manifest 里报哪几个名字，都从这一份断言结果派生：
// 两处各写一遍类型断言的话，加第四个可选接口时必然会出现「注册了但没报」
// （核心不发那条调用，功能静默缺失）或「报了但没注册」（一调就是方法未注册）。
type pluginCaps struct {
	starter host.Starter
	closer  host.Closer
	jobber  host.JobRunner
	// methods 随三个断言一起生成，顺序固定，便于日志与测试逐字比对。
	methods []string
}

func capabilitiesOf(p host.Plugin) pluginCaps {
	var caps pluginCaps
	if v, ok := p.(host.Starter); ok {
		caps.starter = v
		caps.methods = append(caps.methods, OptionalStart)
	}
	if v, ok := p.(host.Closer); ok {
		caps.closer = v
		caps.methods = append(caps.methods, OptionalClose)
	}
	if v, ok := p.(host.JobRunner); ok {
		caps.jobber = v
		caps.methods = append(caps.methods, OptionalRunJob)
	}
	return caps
}

// OptionalMethods 报告 p 实现了哪几个可选接口，供 `--manifest` 的应答
// （见 EncodeManifest）与 ServePlugin 共用，保证探测到的事实与线上一致。
func OptionalMethods(p host.Plugin) []string {
	return capabilitiesOf(p).methods
}

type pluginServer struct {
	c    *Conn
	p    host.Plugin
	caps pluginCaps
	gate chan struct{}

	mu      sync.Mutex
	inited  bool
	initErr error
}

// optional 是「本产物实现了哪几个可选接口」的清单，随 manifest 一起报给核心。
// 与注册方法是同一份断言结果（caps），将来加第四个可选接口时不会出现
// 「注册了但清单里没报」的静默缺口。
func (s *pluginServer) optional() []string {
	return s.caps.methods
}

func (s *pluginServer) manifest(_ context.Context, _ Request) (Response, error) {
	return Response{Result: manifestResult{
		Manifest: toManifestDTO(s.p.Manifest()),
		Optional: s.optional(),
	}}, nil
}

func (s *pluginServer) init(ctx context.Context, req Request) (Response, error) {
	var p initParams
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

	var initErr error
	completed := false
	// gate 必须**一定**关上（失败也关）：否则 Init panic 之后每个
	// handle / close 都挂在 wait 上，插件的表现是「装了但永不理人」。
	defer func() {
		if !completed {
			// 只有 panic 会走到这里：Init 正常返回时 completed 已置位。
			// 记成 Init 的错误，别让后来者以为初始化成功。
			initErr = fmt.Errorf("%w: plugin.init", ErrHandlerPanic)
		}
		s.mu.Lock()
		s.initErr = initErr
		s.mu.Unlock()
		close(s.gate)
	}()

	initErr = s.p.Init(ctx, NewHostClient(s.c, name, vals))
	completed = true
	return Response{}, initErr
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
	var p handleParams
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
		return Response{Result: handleResult{}}, nil
	}
	dto, body := encodeReply(reply)
	return Response{Result: handleResult{Reply: &dto}, Body: body}, nil
}

func (s *pluginServer) start(ctx context.Context, _ Request) (Response, error) {
	if err := s.wait(ctx); err != nil {
		return Response{}, err
	}
	return Response{}, s.caps.starter.Start(ctx)
}

func (s *pluginServer) close(ctx context.Context, _ Request) (Response, error) {
	// Close 不看 initErr：半初始化同样要收摊。
	select {
	case <-s.gate:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	return Response{}, s.caps.closer.Close(ctx)
}

func (s *pluginServer) runJob(ctx context.Context, req Request) (Response, error) {
	if err := s.wait(ctx); err != nil {
		return Response{}, err
	}
	var p runJobParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodPluginRunJob, err)
	}
	return Response{}, s.caps.jobber.RunJob(ctx, p.Spec.toHost())
}

// —— 调用侧：核心怎么驱动一个插件 ——
//
// 这六个方法把 plugin.* 的线格式包成 host 类型，与 HostClient 对称。
// 它们刻意留在这里而不是 core 里：线格式的知识只应存在于一个包，
// 否则 #40 的 proxy 会再写一遍 DTO 映射，两处漂移就是两套协议。

// 超时口径（ADR-0005 决策 7 / §3.6）：**由 RPC 层强制**，不靠调用方记得设。
// 这两条是仅有的两个有既定口径的入口，`Handle` 3 s、`Job` 60 s；其余
// （init / start / close）的时限属于关停序列，由核心在那边定（三级关闭）。
//
// #40 接上 dispatcher 与调度器时应当直接用这两个常量，别再声明第三份
// ——两个数字各写一遍，迟早一边改了另一边没改。
const (
	HandleTimeout = 3 * time.Second
	JobTimeout    = 60 * time.Second
)

// withDefaultTimeout 只在调用方没给截止时间时补一个默认值；给了就照用
// （测试与运维用的长超时不该被压回 3 s）。
func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

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
	var out manifestResult
	if err := res.Decode(&out); err != nil {
		return host.Manifest{}, nil, err
	}
	return out.Manifest.toHost(), out.Optional, nil
}

// CallInit 把配置快照交给插件并让它初始化。cfg 是 [plugins.<name>] 段的原始 map。
func (c *Conn) CallInit(ctx context.Context, name string, cfg map[string]any) error {
	_, err := c.Call(ctx, MethodPluginInit, initParams{Name: name, Config: cfg})
	return err
}

// CallHandle 投递一个事件。返回 nil 表示插件选择不回复（进程内同样的语义）：
// 它与「回复一条空消息」是两件事，所以这里保留 nil。
//
// ctx 没带截止时间时按 HandleTimeout（3 s）兜底——§3.6 的口径由 RPC 层
// 强制，否则最慢的那个插件会把整个 per-peer 队列拖住。
func (c *Conn) CallHandle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	ctx, cancel := withDefaultTimeout(ctx, HandleTimeout)
	defer cancel()
	res, err := c.Call(ctx, MethodPluginHandle, handleParams{Event: toEventDTO(ev)})
	if err != nil {
		return nil, err
	}
	var out handleResult
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

// CallRunJob 执行一个到点的任务。超时优先用调用方设在 ctx 上的
// `JobSpec.Timeout`；调用方没设时按 JobTimeout 兜底，理由与 CallHandle
// 相同——「记得设超时」不该是每个调用点的义务。
func (c *Conn) CallRunJob(ctx context.Context, spec host.JobSpec) error {
	ctx, cancel := withDefaultTimeout(ctx, JobTimeout)
	defer cancel()
	_, err := c.Call(ctx, MethodPluginRunJob, runJobParams{Spec: toJobSpecDTO(spec)})
	return err
}

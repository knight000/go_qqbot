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
//
// 生命周期以 `plugin.init` 为分界线，且**按 Bot 各有一条**（ADR-0008 决策 11：
// 一套插件进程服务所有 Bot，每个 Bot 有独立的业务面配置）：某个 Bot 的
// init 结束（成功或失败）之前，属于它的 handle / start / run_job 一律等；
// 别的 Bot 的事件不受影响——否则一个租户的配置写错会连带别的租户停摆。
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

	s := &pluginServer{
		c:    c,
		p:    p,
		caps: capabilitiesOf(p),
		bots: make(map[string]*botState),
	}

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

// pluginServer 是 plugin.* 的实现。
//
// 每个 Bot 一条独立的初始化门（gate）与一份 HostClient：同一插件实例在
// init 之后要服务多个 Bot，而"这个 Bot 的配置写好了没有"是每个 Bot 自己的
// 状态。把它们塞进一个字段就会让租户之间互相阻塞。
type pluginServer struct {
	c    *Conn
	p    host.Plugin
	caps pluginCaps

	mu   sync.Mutex
	bots map[string]*botState
}

// botState 是一个 Bot 在插件侧的全部状态。
type botState struct {
	host *HostClient
	cfg  map[string]any
	// gate 在该 Bot 的 Init 返回（成功、失败或 panic）时关闭。
	gate   chan struct{}
	closed bool
	err    error
}

// lookup 取已有的 Bot 状态；没有返回 nil（= 这个 Bot 还没 init）。
func (s *pluginServer) lookup(botID string) *botState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bots[botID]
}

// optional 是「本产物实现了哪几个可选接口」的清单，随 manifest 一起报给核心。
// 与注册方法是同一份断言结果（caps），将来加第四个可选接口时不会出现
// 「注册了但清单里没报」的静默缺口。
func (s *pluginServer) optional() []string {
	return s.caps.methods
}

func (s *pluginServer) manifest(_ context.Context, _ Request) (Response, error) {
	// 走 PluginManifest 而不是 s.p.Manifest()：使用面（tools/apisurface 的产物）
	// 在清单出口合并，`--manifest` 那条路也走同一个函数，两处不可能不一致。
	return Response{Result: manifestResult{
		Manifest: toManifestDTO(PluginManifest(s.p)),
		Optional: s.optional(),
	}}, nil
}

// botIDOf 取出 per-Bot 方法里的 bot_id，空着就报错。
//
// 空 bot_id 不能"就当第一个 Bot"：共享进程下挑一个是跨租户串数据；而把它
// 当进程级调用则会静默跳过初始化门。两种猜测都比一次明确的失败坏。
func botIDOf(method, botID string) (string, error) {
	if botID == "" {
		return "", fmt.Errorf("rpc: %s 缺少 bot_id（该方法是 per-Bot 的）", method)
	}
	return botID, nil
}

func (s *pluginServer) init(ctx context.Context, req Request) (Response, error) {
	var p initParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodPluginInit, err)
	}
	botID, err := botIDOf(MethodPluginInit, p.Bot)
	if err != nil {
		return Response{}, err
	}

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

	s.mu.Lock()
	bs, ok := s.bots[botID]
	if ok && bs.closed {
		s.mu.Unlock()
		return Response{}, fmt.Errorf("rpc: Bot %s 的插件已经初始化过（Init 每个 Bot 只调用一次）", botID)
	}
	if !ok {
		bs = &botState{gate: make(chan struct{})}
		s.bots[botID] = bs
	}
	// HostClient 现在就建好：Init 里就要用它（Config 是本地值，其余能力
	// 会带着这个 Bot 的 id 发 RPC）。
	bs.host = NewHostClient(s.c, name, vals, host.BotInfo{AppID: botID, CertType: p.CertType})
	bs.cfg = vals
	s.mu.Unlock()

	var initErr error
	completed := false
	// gate 必须**一定**关上（失败也关）：否则这个 Bot 的 handle / close 都挂在
	// wait 上，插件的表现是「装了但永不理人」。
	defer func() {
		if !completed {
			// 只有 panic 会走到这里：Init 正常返回时 completed 已置位。
			// 记成 Init 的错误，别让后来者以为初始化成功。
			initErr = fmt.Errorf("%w: plugin.init", ErrHandlerPanic)
		}
		s.mu.Lock()
		bs.err = initErr
		bs.closed = true
		s.mu.Unlock()
		close(bs.gate)
	}()

	hctx := host.WithHost(ctx, bs.host)
	initErr = s.p.Init(hctx, bs.host)
	completed = true
	return Response{}, initErr
}

// wait 等某个 Bot 的 Init 结束。ctx 到期时如实返回 ctx 的错误——插件卡在
// Init 里时，核心看到的应该是「超时」，而不是「方法未注册」。
//
// 没有该 Bot 的状态说明 init 还没到：这是协议顺序问题（核心总是先 init 再
// 投递），如实报错而不是无限等。
func (s *pluginServer) wait(ctx context.Context, method, botID string) (*botState, error) {
	bs := s.lookup(botID)
	if bs == nil {
		return nil, fmt.Errorf("rpc: %s 送到了还没初始化的 Bot %s（核心应先 plugin.init）", method, botID)
	}
	select {
	case <-bs.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if bs.err != nil {
		return nil, bs.err
	}
	return bs, nil
}

// hostFor 取本次调用用的 HostClient。正常路径是 init 时建好的那一份；
// 万一（热重启、乱序）没有，就用请求里带的 bot_id + cert_type 现造一个——
// 这也是 handle 带上 CertType 的原因：不必为一次 Bot() 再发一轮 RPC。
func (s *pluginServer) hostFor(bs *botState, botID, certType string) *HostClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bs.host != nil {
		return bs.host
	}
	bs.host = NewHostClient(s.c, s.p.Manifest().Name, bs.cfg, host.BotInfo{AppID: botID, CertType: certType})
	return bs.host
}

func (s *pluginServer) handle(ctx context.Context, req Request) (Response, error) {
	var p handleParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodPluginHandle, err)
	}
	botID, err := botIDOf(MethodPluginHandle, p.Bot)
	if err != nil {
		return Response{}, err
	}
	bs, err := s.wait(ctx, MethodPluginHandle, botID)
	if err != nil {
		return Response{}, err
	}
	// 当前 Bot 的 Host 放进 ctx：插件里存着的那个 h（Init 时给的）在
	// Handle 里调用任何带 ctx 的能力，都会按**这条事件所属的 Bot** 解析。
	hctx := host.WithHost(ctx, s.hostFor(bs, botID, p.CertType))
	reply, err := s.p.Handle(hctx, p.Event.toHost())
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

// start 是 per-Bot 的：后台协程必须按 Bot 各起一个，插件不能用包级变量存
// "当前 Bot"。入参里的 CertType 让核心侧不必再问一次。
func (s *pluginServer) start(ctx context.Context, req Request) (Response, error) {
	var p botParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodPluginStart, err)
	}
	botID, err := botIDOf(MethodPluginStart, p.Bot)
	if err != nil {
		return Response{}, err
	}
	bs, err := s.wait(ctx, MethodPluginStart, botID)
	if err != nil {
		return Response{}, err
	}
	hctx := host.WithHost(ctx, s.hostFor(bs, botID, p.CertType))
	return Response{}, s.caps.starter.Start(hctx)
}

func (s *pluginServer) close(ctx context.Context, _ Request) (Response, error) {
	// Close 不看 initErr：半初始化同样要收摊。它是**进程级**的一次调用，
	// 但要等所有已开始的 Init 落定——否则 close 会与在跑的 Init 撞在
	// 同一份插件状态上。
	s.mu.Lock()
	gates := make([]chan struct{}, 0, len(s.bots))
	for _, bs := range s.bots {
		gates = append(gates, bs.gate)
	}
	s.mu.Unlock()
	for _, g := range gates {
		select {
		case <-g:
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
	return Response{}, s.caps.closer.Close(ctx)
}

func (s *pluginServer) runJob(ctx context.Context, req Request) (Response, error) {
	var p runJobParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return Response{}, badParams(MethodPluginRunJob, err)
	}
	botID, err := botIDOf(MethodPluginRunJob, p.Bot)
	if err != nil {
		return Response{}, err
	}
	bs, err := s.wait(ctx, MethodPluginRunJob, botID)
	if err != nil {
		return Response{}, err
	}
	hctx := host.WithHost(ctx, s.hostFor(bs, botID, ""))
	return Response{}, s.caps.jobber.RunJob(hctx, p.Spec.toHost())
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

// CallInit 把某个 Bot 的配置快照交给插件并让它初始化。cfg 是
// [plugins.<name>] 段的原始 map；bot 是这次握手所属的 Bot。
//
// 共享进程下每个 (Bot, 插件) 各调一次：配置是业务面的、按 Bot 不同
// （ADR-0008 决策 10），一个插件实例因此要按 Bot 各初始化一遍。
func (c *Conn) CallInit(ctx context.Context, bot host.BotInfo, name string, cfg map[string]any) error {
	_, err := c.Call(ctx, MethodPluginInit, initParams{
		Bot: bot.AppID, CertType: bot.CertType, Name: name, Config: cfg,
	})
	return err
}

// CallHandle 投递一个事件。返回 nil 表示插件选择不回复（进程内同样的语义）：
// 它与「回复一条空消息」是两件事，所以这里保留 nil。
//
// bot 是这条事件所属的 Bot：插件侧据此收到正确的那份 Host，`Host.Bot()`
// 与 Host.KV() 都落在它上面。
//
// ctx 没带截止时间时按 HandleTimeout（3 s）兜底——§3.6 的口径由 RPC 层
// 强制，否则最慢的那个插件会把整个 per-peer 队列拖住。
func (c *Conn) CallHandle(ctx context.Context, bot host.BotInfo, ev *host.Event) (*host.Reply, error) {
	ctx, cancel := withDefaultTimeout(ctx, HandleTimeout)
	defer cancel()
	res, err := c.Call(ctx, MethodPluginHandle, handleParams{
		Bot: bot.AppID, CertType: bot.CertType, Event: toEventDTO(ev),
	})
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

// CallStart 调某个 Bot 的 Start。插件没实现时返回的 err 命中 ErrMethodNotFound。
func (c *Conn) CallStart(ctx context.Context, bot host.BotInfo) error {
	_, err := c.Call(ctx, MethodPluginStart, botParams{Bot: bot.AppID, CertType: bot.CertType})
	return err
}

// CallClose 调 Close，用于关停与重启前的收尾。这是**进程级**调用：
// 一个插件进程只有一个 Close，不为 Bot 各来一次。
func (c *Conn) CallClose(ctx context.Context) error {
	_, err := c.Call(ctx, MethodPluginClose, nil)
	return err
}

// CallRunJob 执行一个到点的任务。超时优先用调用方设在 ctx 上的
// `JobSpec.Timeout`；调用方没设时按 JobTimeout 兜底，理由与 CallHandle
// 相同——「记得设超时」不该是每个调用点的义务。
func (c *Conn) CallRunJob(ctx context.Context, bot host.BotInfo, spec host.JobSpec) error {
	ctx, cancel := withDefaultTimeout(ctx, JobTimeout)
	defer cancel()
	_, err := c.Call(ctx, MethodPluginRunJob, runJobParams{
		Bot: bot.AppID, Spec: toJobSpecDTO(spec),
	})
	return err
}

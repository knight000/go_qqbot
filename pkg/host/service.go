package host

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// KVBackend / QueryBackend 由 store.DB 实现。
// 用窄接口而不是直接依赖 *store.DB，是为了让 host 可单测、也避免插件间接受影响。
//
// peer 为空串表示插件级全局键；非空表示该会话私有的键。
type KVBackend interface {
	KVGet(ctx context.Context, plugin, peer, key string) ([]byte, bool, error)
	KVSet(ctx context.Context, plugin, peer, key string, val []byte, ttl time.Duration) error
	KVDelete(ctx context.Context, plugin, peer, key string) error
}

type QueryBackend interface {
	QueryRows(ctx context.Context, query string, args ...any) ([]map[string]any, error)
}

// AdminBackend 由 core 的适配器实现：角色判定与收件人列表都走这里，
// 插件永远拿不到 admins 表的原始访问权。
//
// 角色是全局的：openid 跨场景同值，所以不需要 scene / peer 参数。
type AdminBackend interface {
	AdminRole(ctx context.Context, openid, unionID string) (role string, ok bool, err error)
	AdminTargets(ctx context.Context) ([]AdminTarget, error)
}

// HTTPBackend 是出站 HTTP 的**实现侧**契约：由核心注入（P3 之后就是 RPC 客户端），
// 插件看不到它——插件只看到 HTTP。
//
// 它存在是为了让本包不必 import 实现包（ADR-0005 决策 1）：契约一旦 import
// internal/httpx，等于把一个实现细节永久烧进跨进程格式。
//
// 错误口径与 HTTP 相同：**非 2xx 必须返回 *HTTPStatusError**。翻译在实现方完成
// （它认识 httpx 的类型或线协议的错误码），本包只做转发——契约包不该知道
// 实现侧的错误长什么样。网络失败与 ctx 取消照 Go 原样返回，不得包装。
type HTTPBackend interface {
	GetJSON(ctx context.Context, url string, out any) error
	PostJSON(ctx context.Context, url string, in, out any) error
	GetJSONWithHeader(ctx context.Context, url string, header map[string]string, out any) error
	GetJSONOnce(ctx context.Context, url string, header map[string]string, out any) error
	// WithTimeout 返回共享连接池与全部限流状态、只替换整体超时的视图。
	WithTimeout(d time.Duration) HTTPBackend
}

// Options 是构造插件宿主的输入。HTTP 由核心按插件收窄，其余都来自装配点。
type Options struct {
	Name   string
	Config Params
	Sender Sender
	KV     KVBackend
	DB     QueryBackend
	Admin  AdminBackend
	// HTTP 是该插件的出站客户端，**已按 Manifest.AllowedDomains 收窄**：
	// 白名单判定只有一份实现，在核心的装配处，本包不重复判断。
	// 零值时 HTTP() 每次调用都返回「未初始化」，不会静默放行。
	HTTP  HTTPBackend
	Sched Scheduler
	Log   *slog.Logger
	// Renderer 为零值时由 NewService 换成拒绝性替身，见 Renderer()。
	Renderer Renderer
	// Session 为零值时同样换成拒绝性替身，见 Session()。
	Session SessionBackend
}

// Service 是 Host 的实现，每个插件一份。
type Service struct {
	name      string
	cfg       Params
	kv        *kvStore
	kvBackend KVBackend
	query     *queryStore
	http      *httpClient
	sched     *pluginSched
	log       *slog.Logger
	sender    Sender
	admin     AdminBackend
	renderer  Renderer
	session   Session
}

func NewService(o Options) *Service {
	logger := o.Log
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		name:      o.Name,
		cfg:       o.Config,
		kv:        &kvStore{db: o.KV, plugin: o.Name},
		kvBackend: o.KV,
		query:     &queryStore{db: o.DB},
		http:      &httpClient{raw: o.HTTP},
		sched:     &pluginSched{name: o.Name, inner: o.Sched},
		log:       logger.With("plugin", o.Name),
		sender:    o.Sender,
		admin:     o.Admin,
		renderer:  resolveRenderer(o.Renderer),
		session:   resolveSession(o.Session, o.Name),
	}
}

func (s *Service) Plugin() string       { return s.name }
func (s *Service) Config() Params       { return s.cfg }
func (s *Service) Sender() Sender       { return s.sender }
func (s *Service) KV() KV               { return s.kv }
func (s *Service) Query() QueryStore    { return s.query }
func (s *Service) HTTP() HTTP           { return s.http }
func (s *Service) Scheduler() Scheduler { return s.sched }
func (s *Service) Log() *slog.Logger    { return s.log }

// Renderer 返回注入的渲染能力；未注入时是拒绝性替身，永不为 nil。
func (s *Service) Renderer() Renderer { return s.renderer }

// Session 返回会话接管能力；未注入时是拒绝性替身，永不为 nil。
func (s *Service) Session() Session { return s.session }

// PeerKV 返回该会话私有的 KV 命名空间。
//
// 与 KV() 的唯一区别是多了 peer 维度：`/reset session` 只清这一部分，
// 插件级全局键（KV()）不受影响。会话状态请一律写在这里——
// 写在 KV() 里就等于宣布"重置永远清不掉它"。
func (s *Service) PeerKV(peer string) KV {
	return &kvStore{db: s.kvBackend, plugin: s.name, peer: peer}
}

// IsAdmin 查询 Bot 管理员身份。查不到就是 false（fail-closed）——
// 权限判定失败绝不能放行。只有后端故障才返回错误，让调用方决定是否静默。
//
// 新插件请优先用声明式权限（Manifest/Command.Permission）：核心在路由阶段拦截，
// 插件里不会出现判权分支。这个方法保留给"需要在回调或定时任务里再确认一次"的场景。
func (s *Service) IsAdmin(ctx context.Context, id Identity) (bool, error) {
	if s.admin == nil {
		return false, fmt.Errorf("管理员后端未初始化")
	}
	if id.OpenID == "" {
		return false, nil
	}
	role, ok, err := s.admin.AdminRole(ctx, id.OpenID, id.UnionID)
	if err != nil {
		return false, err
	}
	return ok && Role(role).Level() >= RoleAdmin.Level(), nil
}

// Admins 返回收件人目录。每次调用都重新解析，群认领者补上私聊身份后立即可见。
func (s *Service) Admins() AdminDirectory { return adminDir{s.admin} }

type adminDir struct{ db AdminBackend }

func (d adminDir) Targets(ctx context.Context) ([]AdminTarget, error) {
	if d.db == nil {
		return nil, fmt.Errorf("管理员后端未初始化")
	}
	return d.db.AdminTargets(ctx)
}

type kvStore struct {
	db     KVBackend
	plugin string
	peer   string
}

func (k *kvStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if k.db == nil {
		return nil, false, fmt.Errorf("KV 后端未初始化")
	}
	return k.db.KVGet(ctx, k.plugin, k.peer, key)
}

func (k *kvStore) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if k.db == nil {
		return fmt.Errorf("KV 后端未初始化")
	}
	return k.db.KVSet(ctx, k.plugin, k.peer, key, val, ttl)
}

func (k *kvStore) Delete(ctx context.Context, key string) error {
	if k.db == nil {
		return fmt.Errorf("KV 后端未初始化")
	}
	return k.db.KVDelete(ctx, k.plugin, k.peer, key)
}

type queryStore struct{ db QueryBackend }

// Query 把查询原样交给后端。
//
// 这里**不做** SQL 文本层面的只读判断：`WITH … DELETE` 同样以 with 开头，
// 前缀校验拦不住；按 `" limit "` 判断上限还会把 `LIMIT\n100` 误判成没写
// 上限、在尾巴上再拼一个 LIMIT。两件事的负责方都已下移：
//   - 只读由 store 的只读连接（PRAGMA query_only）保证，写语句在执行时报错；
//   - 行数上限由 store.QueryRows 在取行时截断到 1000。
//
// 因此插件看到的 SQL 与自己写的逐字一致，参数也原样透传。
func (q *queryStore) Query(ctx context.Context, query string, args ...any) ([]Row, error) {
	if q.db == nil {
		return nil, fmt.Errorf("查询后端未初始化")
	}
	raw, err := q.db.QueryRows(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]Row, len(raw))
	for i, m := range raw {
		out[i] = Row(m)
	}
	return out, nil
}

type httpClient struct{ raw HTTPBackend }

// backend 返回注入的出站客户端。零值必须报错而不是放行：
// 插件拿不到错误就会误以为请求成功。
func (h *httpClient) backend() (HTTPBackend, error) {
	if h.raw == nil {
		return nil, fmt.Errorf("HTTP 后端未初始化")
	}
	return h.raw, nil
}

func (h *httpClient) GetJSON(ctx context.Context, url string, out any) error {
	c, err := h.backend()
	if err != nil {
		return err
	}
	return c.GetJSON(ctx, url, out)
}

func (h *httpClient) GetJSONWithHeader(ctx context.Context, url string, header map[string]string, out any) error {
	c, err := h.backend()
	if err != nil {
		return err
	}
	return c.GetJSONWithHeader(ctx, url, header, out)
}

func (h *httpClient) GetJSONOnce(ctx context.Context, url string, header map[string]string, out any) error {
	c, err := h.backend()
	if err != nil {
		return err
	}
	return c.GetJSONOnce(ctx, url, header, out)
}

func (h *httpClient) WithTimeout(d time.Duration) HTTP {
	// 后端缺失时保持"调用即报错"的行为，不要在这里换成 nil 客户端：
	// 插件会因为拿不到错误而误以为请求成功。
	if h.raw == nil {
		return h
	}
	return &httpClient{raw: h.raw.WithTimeout(d)}
}

func (h *httpClient) PostJSON(ctx context.Context, url string, in, out any) error {
	c, err := h.backend()
	if err != nil {
		return err
	}
	return c.PostJSON(ctx, url, in, out)
}

// pluginSched 替插件把 JobSpec.Plugin 填上。
//
// 契约写的是"Plugin 字段由 Host 填充"，但此前 Host 只是把核心的
// Scheduler 原样透传，插件不填就会在注册时被拒。per-peer 任务尤其不能
// 依赖插件自觉：任务 id 是 <plugin>:<name>，填错会让 DisableJobsExcept
// 误停用别人的任务。
type pluginSched struct {
	name  string
	inner Scheduler
}

func (p *pluginSched) Register(spec JobSpec) error {
	if p.inner == nil {
		return fmt.Errorf("调度器未初始化")
	}
	spec.Plugin = p.name
	return p.inner.Register(spec)
}

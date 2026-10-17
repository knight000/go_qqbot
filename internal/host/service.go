package host

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/knight000/qqbot/internal/httpx"
)

var registry struct {
	mu      sync.Mutex
	plugins []Plugin
	names   map[string]struct{}
}

// Register 由插件包的 init() 调用（P1 编译期注册）。
// 重名直接 panic：装配期错误必须立刻暴露，不能带到线上。
func Register(p Plugin) {
	m := p.Manifest()
	if m.Name == "" {
		panic("host: 插件 Manifest.Name 不能为空")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.names == nil {
		registry.names = map[string]struct{}{}
	}
	if _, dup := registry.names[m.Name]; dup {
		panic("host: 插件重名: " + m.Name)
	}
	registry.names[m.Name] = struct{}{}
	registry.plugins = append(registry.plugins, p)
}

// Registered 返回全部已注册插件，顺序为 import 顺序。
func Registered() []Plugin {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	out := make([]Plugin, len(registry.plugins))
	copy(out, registry.plugins)
	return out
}

// KVBackend / QueryBackend 由 store.DB 实现。
// 用窄接口而不是直接依赖 *store.DB，是为了让 host 可单测、也避免插件间接受影响。
type KVBackend interface {
	KVGet(ctx context.Context, plugin, key string) ([]byte, bool, error)
	KVSet(ctx context.Context, plugin, key string, val []byte, ttl time.Duration) error
	KVDelete(ctx context.Context, plugin, key string) error
}

type QueryBackend interface {
	QueryRows(ctx context.Context, query string, args ...any) ([]map[string]any, error)
}

// AdminBackend 由 core 的适配器实现：角色判定与收件人列表都走这里，
// 插件永远拿不到 admins 表的原始访问权。
type AdminBackend interface {
	AdminRole(ctx context.Context, openid, unionID, scene, peer string) (role, scope string, ok bool, err error)
	AdminTargets(ctx context.Context) ([]AdminTarget, error)
}

// Options 是构造插件宿主的输入。Domains 来自插件 Manifest。
type Options struct {
	Name    string
	Config  Params
	Sender  Sender
	KV      KVBackend
	DB      QueryBackend
	Admin   AdminBackend
	HTTP    *httpx.Client
	Sched   Scheduler
	Log     *slog.Logger
	Domains []string
}

// Service 是 Host 的实现，每个插件一份。
type Service struct {
	name   string
	cfg    Params
	kv     *kvStore
	query  *queryStore
	http   *httpClient
	sched  Scheduler
	log    *slog.Logger
	sender Sender
	admin  AdminBackend
}

func NewService(o Options) *Service {
	logger := o.Log
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		name:   o.Name,
		cfg:    o.Config,
		kv:     &kvStore{db: o.KV, plugin: o.Name},
		query:  &queryStore{db: o.DB},
		http:   &httpClient{raw: o.HTTP, domains: o.Domains},
		sched:  o.Sched,
		log:    logger.With("plugin", o.Name),
		sender: o.Sender,
		admin:  o.Admin,
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

// IsAdmin 查询管理员身份。查不到就是 false（fail-closed）——
// 权限判定失败绝不能放行。只有后端故障才返回错误，让调用方决定是否静默。
func (s *Service) IsAdmin(ctx context.Context, id Identity) (bool, error) {
	if s.admin == nil {
		return false, fmt.Errorf("管理员后端未初始化")
	}
	if id.OpenID == "" {
		return false, nil
	}
	_, _, ok, err := s.admin.AdminRole(ctx, id.OpenID, id.UnionID, string(id.Scene), id.PeerID)
	if err != nil {
		return false, err
	}
	return ok, nil
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
}

func (k *kvStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if k.db == nil {
		return nil, false, fmt.Errorf("KV 后端未初始化")
	}
	return k.db.KVGet(ctx, k.plugin, key)
}

func (k *kvStore) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if k.db == nil {
		return fmt.Errorf("KV 后端未初始化")
	}
	return k.db.KVSet(ctx, k.plugin, key, val, ttl)
}

func (k *kvStore) Delete(ctx context.Context, key string) error {
	if k.db == nil {
		return fmt.Errorf("KV 后端未初始化")
	}
	return k.db.KVDelete(ctx, k.plugin, key)
}

type queryStore struct{ db QueryBackend }

func (q *queryStore) Query(ctx context.Context, query string, args ...any) ([]Row, error) {
	if q.db == nil {
		return nil, fmt.Errorf("查询后端未初始化")
	}
	trimmed := strings.TrimSpace(query)
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "select") && !strings.HasPrefix(lower, "with") {
		return nil, fmt.Errorf("只允许只读查询")
	}
	// 简易单语句校验：分号只允许出现在结尾之前（本脚手架按最保守规则处理）
	if strings.Contains(strings.TrimSuffix(trimmed, ";"), ";") {
		return nil, fmt.Errorf("只允许单条语句")
	}
	if !strings.Contains(lower, " limit ") {
		trimmed = strings.TrimSuffix(trimmed, ";") + " LIMIT 1000"
	}
	raw, err := q.db.QueryRows(ctx, trimmed, args...)
	if err != nil {
		return nil, err
	}
	out := make([]Row, len(raw))
	for i, m := range raw {
		out[i] = Row(m)
	}
	return out, nil
}

type httpClient struct {
	raw     *httpx.Client
	domains []string
}

func (h *httpClient) client() (*httpx.Client, error) {
	if h.raw == nil {
		return nil, fmt.Errorf("HTTP 后端未初始化")
	}
	return h.raw.WithDomains(h.domains), nil
}

func (h *httpClient) GetJSON(ctx context.Context, url string, out any) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	return c.GetJSON(ctx, url, out)
}

func (h *httpClient) PostJSON(ctx context.Context, url string, in, out any) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	return c.PostJSON(ctx, url, in, out)
}

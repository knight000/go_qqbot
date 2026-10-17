// Package host 定义插件契约与宿主能力。
//
// 核心原则（ARCHITECTURE.md §3.3）：
//   - 插件只拿到 Host 暴露的能力，不直连 SQLite、不直连 WS、不读环境变量取密钥；
//   - 能力清单即权限清单：HTTP 域名白名单来自 Manifest，KV 自动按插件命名空间隔离。
package host

import (
	"context"
	"log/slog"
	"time"
)

// APIVersion 是核心与插件之间的契约版本，不匹配直接拒绝加载。
const APIVersion = "1"

// Scene 是消息场景。频道（guild）留作扩展，核心暂不实现发送。
type Scene string

const (
	SceneGroup Scene = "group"
	SceneC2C   Scene = "c2c"
	SceneGuild Scene = "guild"
)

// 事件类型与平台 t 字段一一对应。
const (
	EventGroupMessage = "GROUP_AT_MESSAGE_CREATE"
	EventC2CMessage   = "C2C_MESSAGE_CREATE"
	EventGroupAdd     = "GROUP_ADD_ROBOT"
	EventGroupDel     = "GROUP_DEL_ROBOT"
)

// Event 是归一化后的事件。插件只看这一层，感知不到 WSS 与 opcode。
type Event struct {
	ID        string // 平台事件/消息 ID，用于幂等
	Type      string // 见 Event* 常量
	Scene     Scene  // group | c2c | guild
	PeerID    string // 群 openid 或用户 openid
	SenderID  string // 发送者 openid
	MsgID     string // 被动回复必须携带
	Content   string
	HasMedia  bool
	Timestamp time.Time
}

func (e *Event) IsMessage() bool {
	return e.Type == EventGroupMessage || e.Type == EventC2CMessage
}

// Reply 是插件的唯一出站形态。
type Reply struct {
	Content string
	MsgType int // 0 text / 2 markdown / 7 media
}

// Sender 负责被动回复与主动消息。msg_seq 由核心分配，插件不得自行指定。
type Sender interface {
	Reply(ctx context.Context, ev *Event, msg Reply) error
	SendActive(ctx context.Context, scene Scene, peerID string, msg Reply) error
}

// Row 是只读查询的一行。
type Row map[string]any

// QueryStore 只读、参数化、行数上限 1000；不允许 DDL/写操作。
type QueryStore interface {
	Query(ctx context.Context, query string, args ...any) ([]Row, error)
}

// KV 是按插件命名空间隔离的键值存储，支持 TTL。
type KV interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// HTTP 是统一出站客户端，域名必须在 Manifest.AllowedDomains 中声明。
type HTTP interface {
	GetJSON(ctx context.Context, url string, out any) error
	PostJSON(ctx context.Context, url string, in, out any) error
}

// JobSpec 是插件注册的定时任务。Plugin 字段由 Host 填充。
type JobSpec struct {
	Plugin  string
	Name    string
	Cron    string
	Timeout time.Duration
	Payload string
}

// ID 返回 <plugin>:<name>，与 jobs 表主键一致。
func (j JobSpec) ID() string { return j.Plugin + ":" + j.Name }

// Scheduler 仅允许在启动期注册任务。
type Scheduler interface {
	Register(spec JobSpec) error
}

// Host 是插件可见的全部能力。
//
// IsAdmin 也是能力项：插件不需要（也不允许）直接查 admins 表来判断权限，
// 只能就"某人在某场景/某 peer 下是不是管理员"向宿主提问。
type Host interface {
	Plugin() string
	Config() Params
	Sender() Sender
	KV() KV
	Query() QueryStore
	HTTP() HTTP
	Scheduler() Scheduler
	// IsAdmin 委托 core 判定管理员身份（owner 隐含全部权限，peer_scope 为空表示全局）。
	IsAdmin(ctx context.Context, scene Scene, openid, peer string) (bool, error)
	Log() *slog.Logger
}

// Command 是声明式路由规则：前缀或正则二选一。
type Command struct {
	Prefix      string
	Pattern     string
	Priority    int
	Description string
}

type Manifest struct {
	Name           string
	Version        string
	APIVersion     string
	Capabilities   []string
	Intents        []string // 订阅的事件类型
	Commands       []Command
	Broadcast      bool
	AllowedDomains []string
	ConfigSchema   map[string]string // 配置项声明，启动时做存在性校验
}

// Plugin 是 P1（编译期注册）插件的生命周期契约。
// Init 之后核心才开始投递事件；Start/Close 为可选接口。
type Plugin interface {
	Manifest() Manifest
	Init(ctx context.Context, h Host) error
	Handle(ctx context.Context, ev *Event) (*Reply, error)
}

// Starter 可选：需要后台协程的插件实现它。
type Starter interface {
	Start(ctx context.Context) error
}

// Closer 可选：优雅退出。
type Closer interface {
	Close(ctx context.Context) error
}

// JobRunner 可选：实现了它，注册的 Job 才会被真正执行。
type JobRunner interface {
	RunJob(ctx context.Context, spec JobSpec) error
}

// Params 是 [plugins.<name>] 段的只读视图。
type Params map[string]any

func (p Params) Str(key, def string) string {
	if v, ok := p[key].(string); ok && v != "" {
		return v
	}
	return def
}

func (p Params) Bool(key string, def bool) bool {
	switch v := p[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	}
	return def
}

func (p Params) Int(key string, def int) int {
	switch v := p[key].(type) {
	case int64:
		return int(v)
	case int:
		return v
	case float64:
		return int(v)
	}
	return def
}

func (p Params) Duration(key string, def time.Duration) time.Duration {
	if s, ok := p[key].(string); ok {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return def
}

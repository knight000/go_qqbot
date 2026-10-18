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
//
// 版本 3：新增全局权限模型（Event.Actor、Command.Permission / DenyReply、
// Manifest.Permission），事件载荷新增群内角色。
const APIVersion = "3"

// Scene 是消息场景。频道（guild）留作扩展，核心暂不实现发送。
type Scene string

const (
	SceneGroup Scene = "group"
	SceneC2C   Scene = "c2c"
	SceneGuild Scene = "guild"
)

// Kind 是事件的大类。它决定该事件要不要落库、能不能进入插件路由。
type Kind string

const (
	KindMessage Kind = "message" // 用户在会话里说的一句话
	KindNotice  Kind = "notice"  // 会话状态或成员关系的变化
	KindRequest Kind = "request" // 需要应答的请求（如申请加群）
	KindMeta    Kind = "meta"    // 平台自身的元事件（订阅授权等）
	KindUnknown Kind = "unknown" // 解码表里没有的事件，只观测、不路由
)

// Notice 给少数"核心必须理解语义"的通知事件一个稳定标签。
//
// 绝大多数通知只有 Name（平台事件名）供观测，核心不解析；但群生命周期的
// 两个事件会改变本地状态（qq_groups 的在群标记），必须有语义标签——
// 否则核心就得比较平台事件名，破坏"平台事件名只出现在 gateway"的边界。
type Notice string

const (
	NoticeNone Notice = ""
	// NoticeBotJoinedGroup：机器人被拉进群（平台事件 GROUP_ADD_ROBOT）。
	NoticeBotJoinedGroup Notice = "bot.joined_group"
	// NoticeBotLeftGroup：机器人被移出群（平台事件 GROUP_DEL_ROBOT）。
	NoticeBotLeftGroup Notice = "bot.left_group"
)

// Event 是归一化后的事件。插件只看这一层，感知不到 WSS 与 opcode。
type Event struct {
	ID       string // 平台事件/消息 ID，用于幂等
	Kind     Kind   // 大类，见 Kind* 常量
	Name     string // 平台原始事件名，只用于观测与排障
	Scene    Scene  // group | c2c | guild；无法判定时为空
	Notice   Notice // 仅 Kind=notice 且有核心语义时非空；插件可读但无需关心
	PeerID   string // 群 openid 或用户 openid
	SenderID string // 发送者 openid（跨场景同值，平台不另给 QQ 号）
	UnionID  string // 平台给的跨场景标识，可能为空
	MsgID    string // 被动回复必须携带
	Content  string
	// MsgType 是平台的 message_type：0 文本 / 3 卡片 / 101 并行 / 102 聊天记录 / 103 引用。
	MsgType   int
	HasMedia  bool
	Payload   []byte // 原始 JSON，仅未知事件保留（排查"平台到底推了什么"）
	Timestamp time.Time

	// Actor 是调用者身份与权限，由核心在投递插件之前填好：网关填群内角色，
	// 核心填 Bot 角色。插件只读——判权不落在插件里，插件漏写检查也不会越权。
	Actor Principal
}

func (e *Event) IsMessage() bool { return e.Kind == KindMessage }

// Identity 是鉴权用的发送者身份。
//
// 权限判定请读 Event.Actor；Identity 保留给"把这个人是谁告诉宿主"这类场景
// （如 Host.IsAdmin、日志）。OpenID 跨场景同值，Bot 角色全局有效，
// 因此 Scene / PeerID 只是上下文，不参与角色判定。
type Identity struct {
	OpenID  string
	UnionID string
	Scene   Scene
	PeerID  string
}

func (e *Event) Identity() Identity {
	return Identity{OpenID: e.SenderID, UnionID: e.UnionID, Scene: e.Scene, PeerID: e.PeerID}
}

// AdminTarget 是"可以主动私聊的管理员"。
// C2COpenID 为空表示该管理员只留下过群身份，平台无法向其发起单聊。
type AdminTarget struct {
	Role      string // owner | admin
	C2COpenID string
	Note      string
}

// AdminDirectory 让插件取到通知收件人，而不用（也不允许）自己查 admins 表。
type AdminDirectory interface {
	Targets(ctx context.Context) ([]AdminTarget, error)
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
// 权限有两条路，推荐前者：
//   - 声明式：Manifest.Permission / Command.Permission，核心在路由阶段拦截，
//     插件代码里不出现任何判权语句；
//   - 命令式：IsAdmin 就"这个人是不是管理员"向宿主提问，只用于展示或
//     非命令场景（如回调里再确认一次）。
type Host interface {
	Plugin() string
	Config() Params
	Sender() Sender
	KV() KV
	// PeerKV 返回该会话私有的 KV 命名空间。会话状态写这里，
	// `/reset session`（R1）才清得掉；写在 KV() 里等于宣布"重置永远清不掉它"。
	PeerKV(peer string) KV
	Query() QueryStore
	HTTP() HTTP
	Scheduler() Scheduler
	// IsAdmin 委托 core 判定 Bot 管理员身份；owner 与 admin 都返回 true。
	// 不判断群内角色——那取决于平台载荷，读 Event.Actor.GroupRole。
	IsAdmin(ctx context.Context, id Identity) (bool, error)
	// Admins 提供启动通知等场景所需的收件人列表（已按单聊 openid 去重）。
	Admins() AdminDirectory
	Log() *slog.Logger
}

// Command 是声明式路由规则：前缀或正则二选一。
type Command struct {
	Prefix      string
	Pattern     string
	Priority    int
	Description string

	// Permission 是执行该命令所需的权限点（任一满足）。零值 = 不限制。
	// 未设置时继承 Manifest.Permission。
	Permission Perm
	// DenyReply 是权限不足时由核心回复的文本；空串表示静默丢弃。
	// 放在核心而不是插件里，是为了让"拒绝"也不依赖插件实现。
	DenyReply string
}

type Manifest struct {
	Name           string
	Version        string
	APIVersion     string
	Capabilities   []string
	Intents        []string // 订阅的事件：`message` 这样的大类，或 `group.message` 这样的场景限定
	Commands       []Command
	Broadcast      bool
	AllowedDomains []string
	ConfigSchema   map[string]string // 配置项声明，启动时做存在性校验

	// Permission 是插件级默认权限：命令自身未声明时继承它。
	// 零值 = 不限制（等价 PermUser）。
	Permission Perm
	// DenyReply 是插件级默认的拒绝语，被 Command.DenyReply 覆盖。
	DenyReply string
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

// Package host 定义插件契约与宿主能力。
//
// 核心原则（ARCHITECTURE.md §3.3）：
//   - 插件只拿到 Host 暴露的能力，不直连 SQLite、不直连 WS、不读环境变量取密钥；
//   - 隔离由核心承担：HTTP 域名白名单来自 Manifest.AllowedDomains，KV 按插件
//     命名空间隔离，权限由 Manifest.Permission 在路由阶段拦截。
//
// Manifest.Capabilities 是保留字段：核心从不读取它，它只是插件作者的自述，
// 既不构成授权也不构成拒绝。别把它当权限清单读（评审 §4.1）。
//
// 本文件里的「评审 §x.y」指 docs/reviews/2026-10-20-nonebot2-review.md 的对应小节。
package host

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/BurntSushi/toml"
)

// APIVersion 是核心与插件之间的契约版本，不匹配直接拒绝加载。
//
// 版本 4：任务支持会话维度（JobSpec.PerPeer / JobSpec.Peer），
// JobRunner 可能收到非空的 Peer。
//
// 版本 5：Host 新增 Renderer()，插件可把 markdown 文档源画成图片。
//
// 版本 6：Host.HTTP 新增 GetJSONWithHeader（带鉴权头）、GetJSONOnce（不重试）、
// WithTimeout（按请求放宽超时，共享连接池与限流状态）。
//
// 版本 7：Host.HTTP 的非 2xx 统一返回 *HTTPStatusError，插件不必 import
// internal/httpx 就能分辨 429 与普通失败。
const APIVersion = "7"

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
	// Username 是平台给的发送者显示名，只用于展示与排障，**不是身份**：
	// 可改、可重复。实测单聊载荷里这个字段存在但恒为空串（C2C 不下发昵称），
	// 所以单聊侧只能显示 openid。判权一律用 SenderID / Actor。
	Username string
	UnionID  string // 平台给的跨场景标识，可能为空
	MsgID    string // 被动回复必须携带
	Content  string
	// MsgType 是平台的 message_type：0 文本 / 3 卡片 / 101 并行 / 102 聊天记录 / 103 引用。
	MsgType int
	// HasMedia 表示平台载荷里带附件。附件不会被下载（入站媒体不做），
	// 所以它只是"这条消息有图/音视频"的标记，别指望能读到内容。
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

// ImageSource 说明一张待发图片的内容从哪里来。
type ImageSource string

const (
	// ImageSourceNone 是零值：表示"这次不带图片"。
	ImageSourceNone ImageSource = ""
	// ImageSourceFile 是本地文件路径，发送时才读取。
	ImageSourceFile ImageSource = "file"
	// ImageSourceBytes 是内存里的图片字节。
	ImageSourceBytes ImageSource = "bytes"
	// ImageSourceURL 是公网地址，由平台自己下载转存。
	ImageSourceURL ImageSource = "url"
)

// Image 是待发送的图片。零值表示没有图片，可直接放进 Reply。
//
// 三个构造函数覆盖三种来源，字段不导出——插件拼不出"既给路径又给 URL"
// 这种非法组合。构造本身不产生 IO：图片在核心真正调用发消息接口之前
// 才读取与上传，所以 file 来源的路径可以稍后才写出来。
//
// 平台限制（官方文档）：二进制直传上限 5 MB，更大的图必须走分片上传；
// 图片格式为 png/jpg，软限 20 MB（超过会降级成文件消息）。
type Image struct {
	src  ImageSource
	path string
	data []byte
	url  string
}

// ImageFromFile 用本地文件构造图片。
func ImageFromFile(path string) Image { return Image{src: ImageSourceFile, path: path} }

// ImageFromBytes 用内存字节构造图片。文件名由核心按内容嗅探生成。
func ImageFromBytes(data []byte) Image { return Image{src: ImageSourceBytes, data: data} }

// ImageFromURL 用公网地址构造图片，要求 http(s) 且可匿名访问——
// 平台侧下载，本机不需要可达。
func ImageFromURL(rawURL string) Image { return Image{src: ImageSourceURL, url: rawURL} }

// Valid 报告是否真的有一张图。零值 Image 返回 false。
func (i Image) Valid() bool { return i.src != ImageSourceNone }

func (i Image) Source() ImageSource { return i.src }
func (i Image) Path() string        { return i.path }
func (i Image) Data() []byte        { return i.data }
func (i Image) URL() string         { return i.url }

// Reply 是插件的唯一出站形态。
//
// Content 与 Image 同时设置时，核心拆成两条消息（先文本、后图片）：
// 平台一条消息只能有一种 msg_type，图文没法合在一条里，代价是消耗两次
// 被动回复配额（群 5 次/5 分钟、单聊 4 次/小时）。拆分的顺序固定，
// 第二条失败时核心会在错误里说明"前 N 条已发出"。
type Reply struct {
	Content string
	MsgType int // 0 text / 2 markdown / 7 media
	Image   Image
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
//
// 所有方法都经过同一套白名单、连接池与进程级限流；`WithTimeout` 返回的视图
// 与调用者共享这些状态，只替换整体超时。
//
// 上游返回非 2xx 时，方法返回 *HTTPStatusError（不是 httpx 的类型）：
// 插件据此分辨「429 → 冷却」与「普通失败」，而且不需要 import internal/httpx。
type HTTP interface {
	GetJSON(ctx context.Context, url string, out any) error
	PostJSON(ctx context.Context, url string, in, out any) error
	// GetJSONWithHeader 与 GetJSON 相同，额外附加一组请求头。
	// 请求头由调用方负责（鉴权、User-Agent 等），核心不解释其含义。
	GetJSONWithHeader(ctx context.Context, url string, header map[string]string, out any) error
	// GetJSONOnce 只尝试一次，不重试。配额由调用方自己记账时必须用它：
	// 出站层的隐式重试不经过业务配额，会把账放大 2–3 倍。
	GetJSONOnce(ctx context.Context, url string, header map[string]string, out any) error
	// WithTimeout 返回共享连接池与全部限流状态、只替换整体超时的视图。
	// 返回接口而不是具体类型，以便与其他修饰器自由串联。
	WithTimeout(d time.Duration) HTTP
}

// HTTPStatusError 表示出站 HTTP 响应非 2xx。
//
// Body 已被核心截断，**仅供排障**：长度与内容都不保证，不要拿它解析业务数据，
// 也不要原样回显给用户。唯一被允许的读取用途是取 Torn 的错误码——Torn 把业务
// 错误放在 body 里，非 2xx 时同样如此。
//
// 网络错误与 ctx 取消不会被包装成它——它们保持 Go 的错误语义（如
// context.DeadlineExceeded），调用方用 errors.Is 区分。
//
// 存在的理由是边界：插件想判 429 就必须读状态码，而读 httpx 的具体类型
// 会把插件绑到核心的内部实现上。所以由 host 翻译，插件只依赖本类型。
type HTTPStatusError struct {
	Status int
	Body   string
}

func (e *HTTPStatusError) Error() string {
	// 只写状态码：URL 的 query 与请求头里的凭据都不该进错误串（会进日志）。
	return fmt.Sprintf("host: HTTP %d", e.Status)
}

// JobSpec 是插件注册的定时任务。Plugin 字段由 Host 填充。
type JobSpec struct {
	Plugin  string
	Name    string
	Cron    string
	Timeout time.Duration
	Payload string

	// PerPeer 让任务按会话展开：核心为每个「机器人当前所在的群」各建一条实例，
	// 执行时 Peer 填目标群。零值 = 全局任务，只注册一行、只执行一次。
	//
	// 展开时机是插件无法自己完成的：它拿不到群列表（也不该拿），
	// 所以由核心在启动、入群、以及首次见到某个群时补建实例。
	PerPeer bool

	// Peer 由核心在执行前填好，插件注册时不要设置它：
	// per-peer 任务指向目标群，全局任务为空。
	Peer string
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
	// Renderer 返回把 markdown 画成图片的能力。永不为 nil：未装配时返回
	// 一个拒绝一切的替身，插件不必写判空分支。
	Renderer() Renderer
}

// Command 是声明式路由规则：前缀或正则二选一。
//
// 没有第三种匹配方式：平台是否 @ 了机器人不参与匹配（网关解出 mentions 后即
// 丢弃，见评审 §4.2）。
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
	Name       string
	Version    string
	APIVersion string
	// Capabilities 是**保留字段**：核心从不读取，不产生任何授权或拒绝。
	// 门控是 Manifest 的另外三项——AllowedDomains、Permission、配置里的主动
	// 消息白名单（评审 §4.1）。删字段要改所有插件的 Manifest，所以保留。
	Capabilities []string
	Intents      []string // 订阅的事件：`message` 这样的大类，或 `group.message` 这样的场景限定
	Commands     []Command
	// Broadcast 让中标插件声明“同优先级的其它插件也执行”。
	//
	// 它只对中标者生效：候选先按 priority 降序、同 priority 按插件名字典序升序
	// 排序（Router.Match），再取第一个有权限的作为中标者（Dispatcher.handle）。
	// 所以名字靠前的 Broadcast=false 会压掉靠后的 Broadcast=true，且没有提示——
	// 已知缺陷，取舍与替代方案见评审 §2.3。
	Broadcast      bool
	AllowedDomains []string
	// ConfigSchema 声明本插件需要 [plugins.<name>] 里的哪些键、各是什么类型，
	// 由核心在装配期（Init 之前）校验，见 Manifest.ValidateConfig（评审 §4.1）。
	//
	// 语义是**声明即必填**：写进来的键必须有值，缺一个就启动失败；有默认值的键
	// 不要写进来，否则等于把默认值变成必填。类型只支持四种，与 Params 的读取
	// 方法一一对应：string / bool / int / duration。空配置段 + 空 schema 依旧
	// 能启动，所以不声明 schema 的插件行为与校验上线前完全一致。
	ConfigSchema map[string]string

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
//
// 带段名是为了让 Decode 的错误能指到具体插件段：配置写错时使用者需要
// 一眼看出是"哪个插件、哪个键"，而不是去猜。
type Params struct {
	name string
	vals map[string]any
}

// NewParams 构造插件配置视图。name 是配置段名（通常等于插件名）。
func NewParams(name string, vals map[string]any) Params {
	return Params{name: name, vals: vals}
}

// Name 返回配置段名。
func (p Params) Name() string { return p.name }

func (p Params) Str(key, def string) string {
	if v, ok := p.vals[key].(string); ok && v != "" {
		return v
	}
	return def
}

func (p Params) Bool(key string, def bool) bool {
	switch v := p.vals[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	}
	return def
}

func (p Params) Int(key string, def int) int {
	switch v := p.vals[key].(type) {
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
	if s, ok := p.vals[key].(string); ok {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return def
}

// Decode 把整个配置段解成结构体，v 必须是非 nil 指针。
//
// 实现沿用仓库已有的 TOML 依赖：先把配置 map 编码回 TOML，再解进结构体。
// 这条路径对配置值的类型无损，也不必为数组与嵌套表手写反射代码——
// 插件不再需要自己「先序列化再反序列化」绕一圈。
//
// 错误带段名与键名（TOML 解码错误自带 "last key ..."），让类型不匹配在
// 启动期就能定位到具体配置项。空配置段返回零值结构体且不报错，
// 默认配置必须能正常启动。本方法只解码、不校验，必填项由插件自己检查。
func (p Params) Decode(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("插件配置段 %s: Decode 需要非 nil 指针", p.label())
	}
	if len(p.vals) == 0 {
		return nil
	}
	data, err := toml.Marshal(p.vals)
	if err != nil {
		return fmt.Errorf("插件配置段 %s: 编码失败: %w", p.label(), err)
	}
	if err := toml.Unmarshal(data, v); err != nil {
		return fmt.Errorf("插件配置段 %s: %w", p.label(), err)
	}
	return nil
}

func (p Params) label() string {
	if p.name == "" {
		return "(未命名)"
	}
	return p.name
}

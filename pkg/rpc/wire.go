package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// 本文件是 pkg/host 与线上 JSON 之间的翻译层。
//
// 为什么另立一套 DTO 而不是给 host 的类型打 json tag（ADR-0005 决策 1）：
// tag 会把「字段名」变成协议的一部分，而字段名在 Go 侧是可以随手改的
// ——重构时谁都不会想到「改个名字等于改协议」。DTO 是显式的：它只在这里
// 出现，改动必须来这里改，而这里只有 JSON 与结构体，没有逻辑。
//
// 另一个理由是私有字段：host.Image 的三个来源字段都不导出（插件拼不出
// 「既给路径又给 URL」这种非法组合），JSON 编不出来，必须经构造函数往返。

// manifestDTO 对应 host.Manifest，含 config_schema。
type manifestDTO struct {
	Name           string
	Version        string
	APIVersion     string
	Capabilities   []string
	Intents        []string
	Commands       []commandDTO
	Yield          bool
	AllowedDomains []string
	ConfigSchema   map[string]string
	Permission     uint32
	DenyReply      string
}

type commandDTO struct {
	Prefix      string
	Loose       bool
	Pattern     string
	Priority    int
	Description string
	Permission  uint32
	DenyReply   string
}

func toManifestDTO(m host.Manifest) manifestDTO {
	d := manifestDTO{
		Name:           m.Name,
		Version:        m.Version,
		APIVersion:     m.APIVersion,
		Capabilities:   m.Capabilities,
		Intents:        m.Intents,
		Yield:          m.Yield,
		AllowedDomains: m.AllowedDomains,
		ConfigSchema:   m.ConfigSchema,
		Permission:     uint32(m.Permission),
		DenyReply:      m.DenyReply,
	}
	if m.Commands != nil {
		d.Commands = make([]commandDTO, len(m.Commands))
		for i, c := range m.Commands {
			d.Commands[i] = commandDTO{
				Prefix:      c.Prefix,
				Loose:       c.Loose,
				Pattern:     c.Pattern,
				Priority:    c.Priority,
				Description: c.Description,
				Permission:  uint32(c.Permission),
				DenyReply:   c.DenyReply,
			}
		}
	}
	return d
}

func (d manifestDTO) toHost() host.Manifest {
	m := host.Manifest{
		Name:           d.Name,
		Version:        d.Version,
		APIVersion:     d.APIVersion,
		Capabilities:   d.Capabilities,
		Intents:        d.Intents,
		Yield:          d.Yield,
		AllowedDomains: d.AllowedDomains,
		ConfigSchema:   d.ConfigSchema,
		Permission:     host.Perm(d.Permission),
		DenyReply:      d.DenyReply,
	}
	if d.Commands != nil {
		m.Commands = make([]host.Command, len(d.Commands))
		for i, c := range d.Commands {
			m.Commands[i] = host.Command{
				Prefix:      c.Prefix,
				Loose:       c.Loose,
				Pattern:     c.Pattern,
				Priority:    c.Priority,
				Description: c.Description,
				Permission:  host.Perm(c.Permission),
				DenyReply:   c.DenyReply,
			}
		}
	}
	return m
}

// eventDTO 对应 host.Event。
//
// Raw / Payload 是 []byte，JSON 会把它们编成 base64（Raw ≤32 KiB，即约
// 43 KiB 的字符串）。它们走 JSON 而不走正文槽：正文槽只有一个，而它必须
// 留给 1–3 MB 的图片字节；把 Raw 也塞进去要么需要第二个槽，要么需要
// 「谁先用谁占」的隐式规则——两者都比多 30% 的编码代价更难维护。
type eventDTO struct {
	ID           string
	Kind         string
	Name         string
	Scene        string
	Notice       string
	PeerID       string
	SenderID     string
	Username     string
	UnionID      string
	MsgID        string
	Content      string
	Command      string
	Args         string
	MsgType      int
	HasMedia     bool
	Mentioned    bool
	Raw          []byte
	RawTruncated bool
	Payload      []byte
	Timestamp    time.Time
	Actor        principalDTO
	Session      *sessionContextDTO
}

type principalDTO struct {
	OpenID    string
	UnionID   string
	Scene     string
	PeerID    string
	Role      string
	GroupRole string
}

type sessionContextDTO struct {
	Plugin string
	State  []byte
}

func toEventDTO(e *host.Event) eventDTO {
	if e == nil {
		return eventDTO{}
	}
	d := eventDTO{
		ID:           e.ID,
		Kind:         string(e.Kind),
		Name:         e.Name,
		Scene:        string(e.Scene),
		Notice:       string(e.Notice),
		PeerID:       e.PeerID,
		SenderID:     e.SenderID,
		Username:     e.Username,
		UnionID:      e.UnionID,
		MsgID:        e.MsgID,
		Content:      e.Content,
		Command:      e.Command,
		Args:         e.Args,
		MsgType:      e.MsgType,
		HasMedia:     e.HasMedia,
		Mentioned:    e.Mentioned,
		Raw:          e.Raw,
		RawTruncated: e.RawTruncated,
		Payload:      e.Payload,
		Timestamp:    e.Timestamp,
		Actor: principalDTO{
			OpenID:    e.Actor.OpenID,
			UnionID:   e.Actor.UnionID,
			Scene:     string(e.Actor.Scene),
			PeerID:    e.Actor.PeerID,
			Role:      string(e.Actor.Role),
			GroupRole: string(e.Actor.GroupRole),
		},
	}
	if e.Session != nil {
		d.Session = &sessionContextDTO{Plugin: e.Session.Plugin, State: e.Session.State}
	}
	return d
}

func (d eventDTO) toHost() *host.Event {
	e := &host.Event{
		ID:           d.ID,
		Kind:         host.Kind(d.Kind),
		Name:         d.Name,
		Scene:        host.Scene(d.Scene),
		Notice:       host.Notice(d.Notice),
		PeerID:       d.PeerID,
		SenderID:     d.SenderID,
		Username:     d.Username,
		UnionID:      d.UnionID,
		MsgID:        d.MsgID,
		Content:      d.Content,
		Command:      d.Command,
		Args:         d.Args,
		MsgType:      d.MsgType,
		HasMedia:     d.HasMedia,
		Mentioned:    d.Mentioned,
		Raw:          d.Raw,
		RawTruncated: d.RawTruncated,
		Payload:      d.Payload,
		Timestamp:    d.Timestamp,
		Actor: host.Principal{
			OpenID:    d.Actor.OpenID,
			UnionID:   d.Actor.UnionID,
			Scene:     host.Scene(d.Actor.Scene),
			PeerID:    d.Actor.PeerID,
			Role:      host.Role(d.Actor.Role),
			GroupRole: host.GroupRole(d.Actor.GroupRole),
		},
	}
	if d.Session != nil {
		e.Session = &host.SessionContext{Plugin: d.Session.Plugin, State: d.Session.State}
	}
	return e
}

// replyDTO 对应 host.Reply。图片字节走正文槽，其余走 JSON。
type replyDTO struct {
	Content string
	MsgType int
	Image   imageDTO
}

type imageDTO struct {
	Source string
	Path   string
	URL    string
}

// encodeImage 把 Image 拆成「元信息 + 正文」。正文只对内存字节源非空。
func encodeImage(img host.Image) (imageDTO, []byte) {
	switch img.Source() {
	case host.ImageSourceFile:
		return imageDTO{Source: string(host.ImageSourceFile), Path: img.Path()}, nil
	case host.ImageSourceBytes:
		return imageDTO{Source: string(host.ImageSourceBytes)}, img.Data()
	case host.ImageSourceURL:
		return imageDTO{Source: string(host.ImageSourceURL), URL: img.URL()}, nil
	default:
		return imageDTO{}, nil
	}
}

// decodeImage 用构造函数还原——插件与核心都不该看见 Image 的私有字段。
//
// 来源为空时返回零值（没有图片），不是错误：Reply 允许只带文本。
func decodeImage(d imageDTO, body []byte) (host.Image, error) {
	switch host.ImageSource(d.Source) {
	case host.ImageSourceNone:
		return host.Image{}, nil
	case host.ImageSourceFile:
		return host.ImageFromFile(d.Path), nil
	case host.ImageSourceBytes:
		return host.ImageFromBytes(body), nil
	case host.ImageSourceURL:
		return host.ImageFromURL(d.URL), nil
	default:
		return host.Image{}, &RemoteError{Code: CodeUnknown, Msg: "rpc: 未知的图片来源 " + strconv.Quote(d.Source)}
	}
}

func encodeReply(r *host.Reply) (replyDTO, []byte) {
	if r == nil {
		return replyDTO{}, nil
	}
	img, body := encodeImage(r.Image)
	return replyDTO{Content: r.Content, MsgType: r.MsgType, Image: img}, body
}

func decodeReply(d replyDTO, body []byte) (*host.Reply, error) {
	img, err := decodeImage(d.Image, body)
	if err != nil {
		return nil, err
	}
	return &host.Reply{Content: d.Content, MsgType: d.MsgType, Image: img}, nil
}

// jobSpecDTO 对应 host.JobSpec。Timeout 用纳秒整数：time.Duration 的 JSON
// 编码就是整数纳秒，但显式写 int64 能让「单位」出现在类型里。
type jobSpecDTO struct {
	Plugin  string
	Name    string
	Cron    string
	Timeout int64
	Payload string
	PerPeer bool
	Peer    string
}

func toJobSpecDTO(s host.JobSpec) jobSpecDTO {
	return jobSpecDTO{
		Plugin:  s.Plugin,
		Name:    s.Name,
		Cron:    s.Cron,
		Timeout: int64(s.Timeout),
		Payload: s.Payload,
		PerPeer: s.PerPeer,
		Peer:    s.Peer,
	}
}

func (d jobSpecDTO) toHost() host.JobSpec {
	return host.JobSpec{
		Plugin:  d.Plugin,
		Name:    d.Name,
		Cron:    d.Cron,
		Timeout: time.Duration(d.Timeout),
		Payload: d.Payload,
		PerPeer: d.PerPeer,
		Peer:    d.Peer,
	}
}

type adminTargetDTO struct {
	Role      string
	C2COpenID string
	Note      string
}

func toAdminTargetDTOs(ts []host.AdminTarget) []adminTargetDTO {
	if ts == nil {
		return nil
	}
	out := make([]adminTargetDTO, len(ts))
	for i, t := range ts {
		out[i] = adminTargetDTO{Role: t.Role, C2COpenID: t.C2COpenID, Note: t.Note}
	}
	return out
}

func (d adminTargetDTO) toHost() host.AdminTarget {
	return host.AdminTarget{Role: d.Role, C2COpenID: d.C2COpenID, Note: d.Note}
}

type identityDTO struct {
	OpenID  string
	UnionID string
	Scene   string
	PeerID  string
}

func toIdentityDTO(id host.Identity) identityDTO {
	return identityDTO{
		OpenID:  id.OpenID,
		UnionID: id.UnionID,
		Scene:   string(id.Scene),
		PeerID:  id.PeerID,
	}
}

func (d identityDTO) toHost() host.Identity {
	return host.Identity{
		OpenID:  d.OpenID,
		UnionID: d.UnionID,
		Scene:   host.Scene(d.Scene),
		PeerID:  d.PeerID,
	}
}

// decodeJSON 是收侧的统一入口：一律 UseNumber。
//
// 默认的 float64 会毁掉整数配置（Params.Int 拿得到 3.0，Params.Decode 把它
// 编回 TOML 时变成 `3.0`，解进 int 字段直接报错）。所以先把数字留在
// json.Number 里，再由 normalizeNums 决定还原成 int64 还是 float64。
func decodeJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}

// normalizeNums 把 json.Number 收敛成 int64 / float64，与进程内一致：
// TOML 给的是 int64，SQLite 给的也是 int64，只有 JSON 会给 float64。
//
// 只在「跨进程保真」有意义的字段上用（配置、行、SQL 参数）；HTTP 的出参
// 故意不走这里——进程内那条路上的 json.Unmarshal 就是 float64，改成 int64
// 反而让「同一个插件在两种形态下行为不同」。
func normalizeNums(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeNums(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = normalizeNums(e)
		}
		return x
	case float64:
		// 整数值的 float64 也还原成 int64：JSON 里 `5` 经 UseNumber 已是
		// json.Number，能走到这里的只有真正的 `5.0` 或别的数的解码结果。
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x)
		}
		return x
	}
	return v
}

// normalizeRows 规范化查询结果里的数字。行类型走 host.Row 而不是
// map[string]any，是为了让「线上给的就是 host.Query 的返回类型」这件事
// 在类型系统里成立——少一次转换，也少一处可以对不齐的地方。
func normalizeRows(rows []host.Row) []host.Row {
	if rows == nil {
		return nil
	}
	out := make([]host.Row, len(rows))
	for i, r := range rows {
		out[i] = host.Row(normalizeNums(map[string]any(r)).(map[string]any))
	}
	return out
}

// —— 每个方法的参数与应答 ——
//
// 这些结构体由**两端共用**：服务侧解码、调用侧编码用的是同一个类型。
// 曾经两侧各写一个匿名 struct、靠 Go 字段名隐式对齐——改一个字段名不会
// 编译失败，只会让对面读出零值，而症状是「连接正常、调用成功、结果是空的」。
// 线格式的知识只放在一个地方，这条才算真的成立。
//
// TTL / Timeout 一律是 int64 纳秒：`time.Duration` 的 JSON 形态本来就是
// 整数纳秒，但把单位写进字段名才不会被当成秒。
type (
	replyParams struct {
		Event eventDTO
		Msg   replyDTO
	}
	sendActiveParams struct {
		Scene string
		Peer  string
		Msg   replyDTO
	}
	// kvParams 被 get / delete 共用；peer 为空即插件级全局键空间。
	kvParams struct {
		Peer string
		Key  string
	}
	kvSetParams struct {
		Peer  string
		Key   string
		Value []byte
		TTL   int64
	}
	kvGetResult struct {
		Value []byte
		Found bool
	}
	queryParams struct {
		Query string
		Args  []any
	}
	queryResult struct {
		Rows []host.Row
	}
	// httpParams 是四个 HTTP 方法的公共入参：字段互有冗余，但一个结构体
	// 比四个只在两三个字段上有差别的结构体好维护。
	httpParams struct {
		URL     string
		In      json.RawMessage
		Header  map[string]string
		Timeout int64 // 纳秒；0 表示用默认超时
	}
	// httpResult 把上游 JSON **原样**带回去，不在核心侧解一遍再编一遍。
	// 解一遍的代价是保真：解进 any 会把所有数字变成 float64（int64 里
	// 超过 2^53 的会被改掉），再编回去就是另一份数据了。
	httpResult struct {
		Out json.RawMessage
	}
	scheduleParams struct {
		Spec jobSpecDTO
	}
	isAdminParams struct {
		ID identityDTO
	}
	isAdminResult struct {
		Admin bool
	}
	adminTargetsResult struct {
		Targets []adminTargetDTO
	}
	renderParams struct {
		Src string
	}
	renderResult struct {
		Image imageDTO
	}
	sessionAwaitParams struct {
		Event eventDTO
		State []byte
		TTL   int64
	}

	// —— plugin.* ——
	initParams struct {
		Name   string
		Config map[string]any
	}
	handleParams struct {
		Event eventDTO
	}
	handleResult struct {
		Reply *replyDTO
	}
	runJobParams struct {
		Spec jobSpecDTO
	}
)

// manifestResult 是清单的**唯一**线格式：`bin --manifest` 打到 stdout 的、
// 以及 plugin.manifest 应答里的，都是这一份（#39 的教训：两侧各写一份 DTO，
// 字段名漂移不会编译失败，只会让对面读出零值）。
//
//	{ "Manifest": {...}, "Optional": ["plugin.start", ...] }
//
// 两条路都只经 EncodeManifest / DecodeManifest，本类型不导出。
type manifestResult struct {
	Manifest manifestDTO
	Optional []string
}

// EncodeManifest 把清单编成线格式。探测侧（插件 `--manifest`）与
// plugin.manifest 的应答都走这里，因此两处不可能出现字段名差异。
func EncodeManifest(m host.Manifest, optional []string) ([]byte, error) {
	return json.Marshal(manifestResult{Manifest: toManifestDTO(m), Optional: optional})
}

// DecodeManifest 解析线格式的清单。
//
// 用 json.Unmarshal 而不是 decodeJSON：它拒绝**尾随内容**。插件往 stdout
// 打一行日志再接 JSON 是最常见的形态，而宽松解析会把前半截当清单、后半截
// 静默丢掉——那会表现为"插件装上了但命令全不响应"，没有任何报错。
func DecodeManifest(b []byte) (host.Manifest, []string, error) {
	var out manifestResult
	if err := json.Unmarshal(b, &out); err != nil {
		return host.Manifest{}, nil, fmt.Errorf("rpc: 插件清单不是合法 JSON: %w", err)
	}
	return out.Manifest.toHost(), out.Optional, nil
}

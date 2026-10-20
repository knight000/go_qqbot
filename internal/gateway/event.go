package gateway

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
)

const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

// 平台事件名只在本包出现：核心与插件看到的是归一化后的 host.Event。
const (
	platGroupAtMessage    = "GROUP_AT_MESSAGE_CREATE" // @机器人
	platGroupMessageAll   = "GROUP_MESSAGE_CREATE"    // 全量模式：群里每条消息都推
	platC2CMessage        = "C2C_MESSAGE_CREATE"
	platGroupAddRobot     = "GROUP_ADD_ROBOT"
	platGroupDelRobot     = "GROUP_DEL_ROBOT"
	platGroupJoinRequest  = "GROUP_JOIN_REQUEST"
	platGroupMsgReceive   = "GROUP_MSG_RECEIVE"
	platGroupMsgReject    = "GROUP_MSG_REJECT"
	platGroupMemberAdd    = "GROUP_MEMBER_ADD"
	platGroupMemberRemove = "GROUP_MEMBER_REMOVE"
	platC2CMsgReceive     = "C2C_MSG_RECEIVE"
	platC2CMsgReject      = "C2C_MSG_REJECT"
	platFriendAdd         = "FRIEND_ADD"
	platFriendDel         = "FRIEND_DEL"
	platInteractionCreate = "INTERACTION_CREATE"
	platSubscribeStatus   = "SUBSCRIBE_MESSAGE_STATUS"
)

// messageEvents 是三个消息类事件到场景的映射。
// GROUP_AT_MESSAGE_CREATE 与 GROUP_MESSAGE_CREATE 字段结构一致，
// 归一化为同一类事件：路由与订阅都不区分来源。两者的差异只落在语义标记上
// （Name 供观测，Mentioned 表示"@ 了我"，见 mentionsBot）。
var messageEvents = map[string]host.Scene{
	platGroupAtMessage:  host.SceneGroup,
	platGroupMessageAll: host.SceneGroup,
	platC2CMessage:      host.SceneC2C,
}

// eventSpec 是非消息事件的归一化结果。
type eventSpec struct {
	kind  host.Kind
	scene host.Scene
	// resolve 为 true 时，若 scene 为空则按载荷里的 group_openid / openid 推断，
	// 用于 INTERACTION_CREATE 这种群聊单聊都会出现的事件。
	resolve bool
}

// eventTable 是本地解码表。新增平台事件只需要在这里加一行。
var eventTable = map[string]eventSpec{
	platGroupAddRobot:     {host.KindNotice, host.SceneGroup, false},
	platGroupDelRobot:     {host.KindNotice, host.SceneGroup, false},
	platGroupJoinRequest:  {host.KindRequest, host.SceneGroup, false},
	platGroupMsgReceive:   {host.KindNotice, host.SceneGroup, false},
	platGroupMsgReject:    {host.KindNotice, host.SceneGroup, false},
	platGroupMemberAdd:    {host.KindNotice, host.SceneGroup, false},
	platGroupMemberRemove: {host.KindNotice, host.SceneGroup, false},
	platC2CMsgReceive:     {host.KindNotice, host.SceneC2C, false},
	platC2CMsgReject:      {host.KindNotice, host.SceneC2C, false},
	platFriendAdd:         {host.KindNotice, host.SceneC2C, false},
	platFriendDel:         {host.KindNotice, host.SceneC2C, false},
	platInteractionCreate: {host.KindMeta, "", true},
	platSubscribeStatus:   {host.KindMeta, "", false},
}

// noticeSemantics 是"核心必须理解语义"的通知白名单。
//
// 其余通知核心一律不解析——它们只带 Kind/Name 供观测，加进来就要分析载荷，
// 那是插件的事。这两个则不同：它们直接改变 qq_groups 的在群状态。
var noticeSemantics = map[string]host.Notice{
	platGroupAddRobot: host.NoticeBotJoinedGroup,
	platGroupDelRobot: host.NoticeBotLeftGroup,
}

type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  int64           `json:"s"`
	T  string          `json:"t"`
	ID string          `json:"id"`
}

// author 是消息发送者。平台文档（group_message_create / c2c_message_create）里
// id 是"用户唯一标识（OpenID 格式）"，member_openid / user_openid 分别标注为
// 群聊 / 单聊场景使用；实测三者对同一个人取同一个值。因此统一以 id 为准，
// 场景字段只作回退——跨场景识别不需要 union_openid 也能成立。
type author struct {
	ID           string `json:"id"`
	MemberOpenID string `json:"member_openid"`
	UserOpenID   string `json:"user_openid"`
	UnionOpenID  string `json:"union_openid"`
	// MemberRole 是群内角色：member / admin / owner。
	// 平台在消息事件载荷里直接给出，不需要额外调接口（详见 ARCHITECTURE §8.1）。
	MemberRole string `json:"member_role"`
	Username   string `json:"username"`
	Bot        bool   `json:"bot"`
}

type messageData struct {
	ID          string            `json:"id"`
	Content     string            `json:"content"`
	GroupOpenID string            `json:"group_openid"`
	Author      author            `json:"author"`
	MsgType     int               `json:"message_type"`
	Timestamp   string            `json:"timestamp"`
	Attachments []json.RawMessage `json:"attachments"`
	// Mentions 是"@ 了别人"的列表，按官方文档**不含@机器人自身**。
	// 因此它不参与 Mentioned 的判定（见 mentionsBot）；需要这些数据的插件
	// 从 Event.Raw 里读，标准字段保持不变只为说明结构。
	Mentions []json.RawMessage `json:"mentions"`
}

// genericData 覆盖通知/请求/元事件的公共字段。平台各事件的字段并不统一，
// 这里取并集，取不到的留空——未知字段不会导致解码失败。
type genericData struct {
	ID             string          `json:"id"`
	GroupOpenID    string          `json:"group_openid"`
	OpenID         string          `json:"openid"`
	UserOpenID     string          `json:"user_openid"`
	MemberOpenID   string          `json:"member_openid"`
	OpMemberOpenID string          `json:"op_member_openid"`
	UnionOpenID    string          `json:"union_openid"`
	MemberRole     string          `json:"member_role"`
	Timestamp      json.RawMessage `json:"timestamp"`
}

// Decode 把平台事件归一化为 host.Event。
//
// 三个出口：
//   - 消息类（3 个事件）→ Kind=message，解析正文与附件；
//   - 解码表内的事件 → Kind=notice/request/meta；
//   - 表外的事件 → Kind=unknown，保留原始 JSON 供观测，永不进入路由。
//
// 三个出口都会带上 Raw（≤ host.MaxRawBytes 的原文副本，超出则截断并置
// RawTruncated）：已知事件解析后原文也要留给插件，否则平台加一个字段就得
// 改核心 + 升契约版本，插件无法自救（票 #13）。
//
// 只有载荷 JSON 损坏时才返回 error——那说明本地结构体与平台不符，属于需要修代码的情况。
func Decode(t string, raw json.RawMessage) (*host.Event, error) {
	var (
		ev  *host.Event
		err error
	)
	scene, isMessage := messageEvents[t]
	spec, isKnown := eventTable[t]
	switch {
	case isMessage:
		ev, err = decodeMessage(t, scene, raw)
	case isKnown:
		ev, err = decodeGeneric(t, spec, raw)
	default:
		ev = &host.Event{
			Kind:      host.KindUnknown,
			Name:      t,
			Payload:   append([]byte(nil), raw...),
			Timestamp: time.Now(),
		}
	}
	if err != nil {
		return nil, err
	}
	ev.Raw, ev.RawTruncated = copyRaw(raw)
	return ev, nil
}

// copyRaw 生成原文副本并按上限截断。
//
// 副本而不是切片：入参 RawMessage 可能指向一整个 WS 帧的大 buffer，
// 截一段留着会把整帧一起钉在内存里。截断时多分配一份上限大小的 buffer，
// 但 RawTruncated 让下游知道它不完整。
// mentionsBot 判定"这条消息 @ 了机器人"。
//
// 唯一可靠的口径是平台事件类型，不是载荷里的 mentions：官方文档对
// GROUP_AT_MESSAGE_CREATE 的 mentions 字段原文是「消息中@的用户列表
// （不含@机器人自身）」——数组里永远不会有机器人自己，反推必然错。
// GROUP_MESSAGE_CREATE（全量模式）载荷里也没有任何「@ 了机器人」的标记，
// 所以为 false；单聊与其它事件在一开始就不是消息路径，恒为 false。
//
// 平台事件名只在本包出现，核心与插件看到的是布尔语义（ARCHITECTURE §1.4）。
func mentionsBot(t string) bool {
	return t == platGroupAtMessage
}

func copyRaw(raw json.RawMessage) ([]byte, bool) {
	if len(raw) <= host.MaxRawBytes {
		return append([]byte(nil), raw...), false
	}
	out := make([]byte, host.MaxRawBytes)
	copy(out, raw[:host.MaxRawBytes])
	return out, true
}

func decodeMessage(t string, scene host.Scene, raw json.RawMessage) (*host.Event, error) {
	var d messageData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", t, err)
	}
	ev := &host.Event{
		ID:        d.ID,
		Kind:      host.KindMessage,
		Name:      t,
		Scene:     scene,
		Username:  d.Author.Username,
		UnionID:   d.Author.UnionOpenID,
		MsgID:     d.ID,
		Content:   normalizeContent(d.Content),
		MsgType:   d.MsgType,
		HasMedia:  len(d.Attachments) > 0,
		Mentioned: mentionsBot(t),
		Timestamp: parseTime(d.Timestamp),
	}
	if scene == host.SceneGroup {
		ev.PeerID = d.GroupOpenID
	} else {
		ev.PeerID = firstNonEmpty(d.Author.UserOpenID, d.Author.ID)
	}
	// 优先 author.id：它是平台声明的"用户唯一标识"，群/单聊同值，
	// 这样 admins 表里的授权不随场景漂移。
	ev.SenderID = firstNonEmpty(d.Author.ID, d.Author.MemberOpenID, d.Author.UserOpenID)
	if ev.SenderID == "" {
		ev.SenderID = ev.PeerID
	}

	// 群内角色只对群消息有意义；单聊留空（未知）。
	if scene == host.SceneGroup {
		ev.Actor = host.Principal{
			OpenID:    ev.SenderID,
			UnionID:   ev.UnionID,
			Scene:     scene,
			PeerID:    ev.PeerID,
			GroupRole: host.ParseGroupRole(d.Author.MemberRole),
		}
	}
	return ev, nil
}

// normalizeContent 把行结束符统一成 `\n`，并去掉首尾空白。
//
// 平台实测（2026-10-19，raw 轨迹取证）：群消息里多行内容的换行是 CR（U+000D），
// 不是 LF。原样透传的后果不只是观感——按 `\n` 切行的下游（渲染器、任何读正文的
// 插件）会把整段当成一行。这是平台的编码差异而非业务语义，归一化归网关，
// 核心与插件只处理 LF。
func normalizeContent(s string) string {
	if strings.ContainsRune(s, '\r') {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
	}
	return strings.TrimSpace(s)
}

func decodeGeneric(t string, spec eventSpec, raw json.RawMessage) (*host.Event, error) {
	var d genericData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", t, err)
	}
	scene := spec.scene
	if scene == "" && spec.resolve {
		switch {
		case d.GroupOpenID != "":
			scene = host.SceneGroup
		case d.OpenID != "" || d.UserOpenID != "":
			scene = host.SceneC2C
		}
	}
	ev := &host.Event{
		ID:        firstNonEmpty(d.ID, stableOpID(t, d)),
		Kind:      spec.kind,
		Name:      t,
		Notice:    noticeSemantics[t],
		Scene:     scene,
		PeerID:    firstNonEmpty(d.GroupOpenID, d.OpenID, d.UserOpenID),
		SenderID:  firstNonEmpty(d.OpMemberOpenID, d.MemberOpenID, d.OpenID, d.UserOpenID),
		UnionID:   d.UnionOpenID,
		Timestamp: parseAnyTime(d.Timestamp),
	}
	// 通知/请求事件多数不带 member_role；能解出来就带上，解不出就是未知，
	// 依赖群内角色的权限点会因此 fail-closed。
	ev.Actor = host.Principal{
		OpenID:    ev.SenderID,
		UnionID:   ev.UnionID,
		Scene:     scene,
		PeerID:    ev.PeerID,
		GroupRole: host.ParseGroupRole(d.MemberRole),
	}
	return ev, nil
}

// stableOpID 给没有平台 ID 的通知事件造一个稳定 ID，保证重连补发时能被去重表拦下。
func stableOpID(t string, d genericData) string {
	ts := strings.Trim(string(d.Timestamp), `"`)
	if ts == "" {
		return ""
	}
	return t + ":" + firstNonEmpty(d.GroupOpenID, d.OpenID, d.UserOpenID) +
		":" + firstNonEmpty(d.OpMemberOpenID, d.MemberOpenID) + ":" + ts
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Now()
}

// parseAnyTime 兼容通知事件里两种时间写法：RFC3339 字符串与 Unix 秒/毫秒。
func parseAnyTime(raw json.RawMessage) time.Time {
	s := strings.Trim(string(raw), `"`)
	if s == "" || s == "null" {
		return time.Now()
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Now()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

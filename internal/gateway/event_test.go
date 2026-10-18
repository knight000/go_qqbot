package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/host"
)

// 真实的群消息事件体（字段取自官方文档示例：id 与 member_openid 同值，
// 另加 union_openid 与 mentions）。
const groupMessagePayload = `{
  "id": "ROBOT1.0_abc",
  "author": {
    "id": "M1",
    "member_openid": "M1",
    "member_role": "member",
    "username": "小明",
    "bot": false,
    "union_openid": "U1"
  },
  "content": "  你好 ",
  "group_openid": "G1",
  "message_type": 0,
  "timestamp": "2026-07-21T08:00:00+08:00",
  "attachments": [{"content_type": "image/jpeg", "url": "https://example.com/a.jpg"}],
  "mentions": [{"id": "X1", "username": "小红"}],
  "message_scene": {"source": "default"}
}`

// 全量模式（GROUP_MESSAGE_CREATE）与 @机器人（GROUP_AT_MESSAGE_CREATE）必须
// 归一化成同一个事件：下游路由与插件订阅都不区分来源。
func TestDecodeGroupMessageNormalizesBothEvents(t *testing.T) {
	atEv, err := Decode(platGroupAtMessage, json.RawMessage(groupMessagePayload))
	if err != nil {
		t.Fatalf("%s 解码失败: %v", platGroupAtMessage, err)
	}
	allEv, err := Decode(platGroupMessageAll, json.RawMessage(groupMessagePayload))
	if err != nil {
		t.Fatalf("%s 解码失败: %v", platGroupMessageAll, err)
	}
	// Name 记录平台原名，是两者唯一允许的差异。
	allEv.Name = atEv.Name
	if !reflect.DeepEqual(atEv, allEv) {
		t.Fatalf("两种群消息事件除 Name 外应完全一致:\n@  %+v\n全量 %+v", atEv, allEv)
	}
}

func TestDecodeGroupMessageFields(t *testing.T) {
	for _, typ := range []string{platGroupAtMessage, platGroupMessageAll} {
		ev, err := Decode(typ, json.RawMessage(groupMessagePayload))
		if err != nil {
			t.Fatalf("%s 解码失败: %v", typ, err)
		}
		if ev.Kind != host.KindMessage || !ev.IsMessage() {
			t.Fatalf("%s 的 Kind 应为 message，实际 %s", typ, ev.Kind)
		}
		if ev.Name != typ {
			t.Fatalf("Name 应保留平台原名 %s，实际 %s", typ, ev.Name)
		}
		if ev.Scene != host.SceneGroup {
			t.Fatalf("场景应为 group，实际 %s", ev.Scene)
		}
		if ev.ID != "ROBOT1.0_abc" || ev.MsgID != "ROBOT1.0_abc" {
			t.Fatalf("ID/MsgID 应取平台消息 id，实际 %q/%q", ev.ID, ev.MsgID)
		}
		if ev.PeerID != "G1" {
			t.Fatalf("PeerID 应为 group_openid，实际 %q", ev.PeerID)
		}
		if ev.SenderID != "M1" || ev.UnionID != "U1" {
			t.Fatalf("发送者/union 错误: %q/%q", ev.SenderID, ev.UnionID)
		}
		// 群内角色必须原样带进权限视图：这是"群主/群管"权限的唯一来源。
		if ev.Actor.GroupRole != host.GroupRoleMember {
			t.Fatalf("GroupRole 应为 member，实际 %q", ev.Actor.GroupRole)
		}
		if ev.Actor.OpenID != ev.SenderID || ev.Actor.PeerID != ev.PeerID {
			t.Fatalf("Actor 身份字段应与事件一致: %+v", ev.Actor)
		}
		if ev.Content != "你好" {
			t.Fatalf("content 应去除首尾空白，实际 %q", ev.Content)
		}
		if ev.MsgType != 0 {
			t.Fatalf("message_type 应写入 MsgType，实际 %d", ev.MsgType)
		}
		if !ev.HasMedia {
			t.Fatal("带 attachments 的消息 HasMedia 应为 true")
		}
		if y, h := ev.Timestamp.Year(), ev.Timestamp.Hour(); y != 2026 || h != 8 {
			t.Fatalf("时间戳解析错误: %s", ev.Timestamp)
		}
	}
}

// 解码表里的每个事件都必须能解出约定的大类与场景——这张表就是"本地解码"的清单。
func TestDecodeTableCoversKnownEvents(t *testing.T) {
	const groupOp = `{"group_openid":"G1","op_member_openid":"M1","timestamp":1721500000}`
	cases := []struct {
		name  string
		raw   string
		kind  host.Kind
		scene host.Scene
	}{
		{platGroupAddRobot, groupOp, host.KindNotice, host.SceneGroup},
		{platGroupDelRobot, groupOp, host.KindNotice, host.SceneGroup},
		{platGroupJoinRequest, `{"group_openid":"G1","member_openid":"M2","timestamp":1721500000}`, host.KindRequest, host.SceneGroup},
		{platGroupMsgReceive, groupOp, host.KindNotice, host.SceneGroup},
		{platGroupMsgReject, groupOp, host.KindNotice, host.SceneGroup},
		{platGroupMemberAdd, `{"group_openid":"G1","member_openid":"M2","op_member_openid":"M1","timestamp":1721500000}`, host.KindNotice, host.SceneGroup},
		{platGroupMemberRemove, `{"group_openid":"G1","member_openid":"M2","op_member_openid":"M1","timestamp":1721500000}`, host.KindNotice, host.SceneGroup},
		{platC2CMsgReceive, `{"openid":"U1","timestamp":1721500000}`, host.KindNotice, host.SceneC2C},
		{platC2CMsgReject, `{"openid":"U1","timestamp":1721500000}`, host.KindNotice, host.SceneC2C},
		{platFriendAdd, `{"openid":"U1","timestamp":1721500000}`, host.KindNotice, host.SceneC2C},
		{platFriendDel, `{"openid":"U1","timestamp":1721500000}`, host.KindNotice, host.SceneC2C},
		{platInteractionCreate, `{"id":"i1","group_openid":"G1","timestamp":"2026-07-21T08:00:00+08:00"}`, host.KindMeta, host.SceneGroup},
		{platSubscribeStatus, `{"openid":"U1","timestamp":1721500000}`, host.KindMeta, ""},
	}
	for _, c := range cases {
		ev, err := Decode(c.name, json.RawMessage(c.raw))
		if err != nil {
			t.Fatalf("%s 解码失败: %v", c.name, err)
		}
		if ev.Name != c.name {
			t.Fatalf("%s 的 Name 错误: %q", c.name, ev.Name)
		}
		if ev.Kind != c.kind || ev.Scene != c.scene {
			t.Fatalf("%s 归一化结果错误: kind=%s scene=%q，期望 %s/%q",
				c.name, ev.Kind, ev.Scene, c.kind, c.scene)
		}
		if ev.ID == "" {
			t.Fatalf("%s 应生成稳定事件 ID", c.name)
		}
	}
}

// 表外事件不报错、不丢弃，而是带着原始载荷变成 unknown——它只用于观测，永不进路由。
func TestDecodeUnknownEventKeepsPayload(t *testing.T) {
	raw := `{"group_openid":"G1","whatever":"x"}`
	ev, err := Decode("SOME_FUTURE_EVENT", json.RawMessage(raw))
	if err != nil {
		t.Fatalf("未知事件不应返回错误: %v", err)
	}
	if ev.Kind != host.KindUnknown {
		t.Fatalf("未知事件应归为 unknown，实际 %s", ev.Kind)
	}
	if ev.Name != "SOME_FUTURE_EVENT" {
		t.Fatalf("Name 应保留原始事件名，实际 %q", ev.Name)
	}
	if string(ev.Payload) != raw {
		t.Fatalf("原始载荷未被保留: %s", ev.Payload)
	}
	if ev.IsMessage() {
		t.Fatal("未知事件不应被当成消息")
	}
}

func TestDecodeC2CMessage(t *testing.T) {
	raw := `{"id":"c1","content":" hi ","timestamp":"2026-07-21T09:30:00+08:00",
	         "message_type":3,
	         "author":{"user_openid":"UO1","union_openid":"U1"}}`
	ev, err := Decode(platC2CMessage, json.RawMessage(raw))
	if err != nil {
		t.Fatalf("单聊消息解码失败: %v", err)
	}
	if ev.Kind != host.KindMessage || ev.Scene != host.SceneC2C {
		t.Fatalf("单聊归一化错误: %+v", ev)
	}
	if ev.PeerID != "UO1" || ev.SenderID != "UO1" {
		t.Fatalf("单聊对端应为 user_openid，实际 %q/%q", ev.PeerID, ev.SenderID)
	}
	if ev.Content != "hi" || ev.MsgType != 3 {
		t.Fatalf("正文或消息类型错误: %q/%d", ev.Content, ev.MsgType)
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	for _, typ := range []string{platGroupAtMessage, platGroupMessageAll, platC2CMessage, platGroupDelRobot} {
		if ev, err := Decode(typ, json.RawMessage(`{"id":`)); err == nil || ev != nil {
			t.Fatalf("%s 遇到坏 JSON 应返回错误: %+v", typ, ev)
		}
	}
}

func TestDecodeNoticeStableID(t *testing.T) {
	raw := `{"group_openid":"G1","op_member_openid":"M1","timestamp":1721500000}`
	ev, err := Decode(platGroupDelRobot, json.RawMessage(raw))
	if err != nil {
		t.Fatalf("退群事件解码失败: %v", err)
	}
	if ev.ID != "GROUP_DEL_ROBOT:G1:M1:1721500000" {
		t.Fatalf("稳定 ID 不符: %q", ev.ID)
	}
	if !strings.HasPrefix(ev.ID, platGroupDelRobot) {
		t.Fatalf("稳定 ID 应带事件名前缀: %q", ev.ID)
	}
}

// 平台文档把 author.id 定义为"用户唯一标识"，member_openid / user_openid 是场景字段。
// 两者不一致时以 id 为准——这决定了 admins 表里的授权会不会随场景漂移。
func TestDecodeGroupMessagePrefersAuthorIDAndKeepsRole(t *testing.T) {
	const raw = `{
	  "id": "MSG1",
	  "author": {"id":"U-SAME","member_openid":"M-GROUP","member_role":"owner","username":"小红"},
	  "content": "hi",
	  "group_openid": "G1",
	  "message_type": 0,
	  "timestamp": "2026-07-21T09:30:00+08:00"
	}`
	ev, err := Decode(platGroupMessageAll, json.RawMessage(raw))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if ev.SenderID != "U-SAME" {
		t.Fatalf("SenderID 应优先取 author.id，实际 %q", ev.SenderID)
	}
	if ev.Actor.GroupRole != host.GroupRoleOwner {
		t.Fatalf("群主应解出 owner，实际 %q", ev.Actor.GroupRole)
	}
}

// 单聊载荷没有 member_role：群内角色必须保持未知，不能让私聊用户凭空获得群管身份。
func TestDecodeC2CHasNoGroupRole(t *testing.T) {
	const raw = `{"id":"MSG2","author":{"id":"U1","user_openid":"U1"},"content":"hi","message_type":0,"timestamp":"2026-07-21T08:00:00+08:00"}`
	ev, err := Decode(platC2CMessage, json.RawMessage(raw))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if ev.Scene != host.SceneC2C || ev.SenderID != "U1" {
		t.Fatalf("单聊字段错误: scene=%q sender=%q", ev.Scene, ev.SenderID)
	}
	if ev.Actor.GroupRole != host.GroupRoleUnknown {
		t.Fatalf("单聊不应有群内角色，实际 %q", ev.Actor.GroupRole)
	}
}

// 平台下发的多行内容是单独 CR（2026-10-19 raw 轨迹取证：`"content":"/md # 标题\r## 二级标题"`），
// 不是 LF。必须在这里统一成 LF，否则按 `\n` 切行的下游会把整段当成一行。
func TestDecodeNormalizesLineEndings(t *testing.T) {
	const raw = `{"id":"MSG3","group_openid":"G1","author":{"id":"U1","member_openid":"M1","member_role":"member"},"content":"a\rb\r\nc\rd\n","message_type":0,"timestamp":"2026-10-19T08:00:00+08:00"}`
	ev, err := Decode(platGroupMessageAll, json.RawMessage(raw))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	// CR、CRLF 都变成 LF，首尾空白（含结尾的 LF）被去掉，内部换行保留。
	if want := "a\nb\nc\nd"; ev.Content != want {
		t.Fatalf("content=%q, want %q", ev.Content, want)
	}
}

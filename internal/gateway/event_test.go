package gateway

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/knight000/go_qqbot/pkg/host"
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
// 归一化成同一个**事件类**：下游路由与插件订阅都不区分来源。
// 差异只有两处，且都必须是语义标记而不是"平台事件名"：Name（观测用）
// 与 Mentioned（"这条消息 @ 了我"这一事实，票 #13）。
func TestDecodeGroupMessageNormalizesBothEvents(t *testing.T) {
	atEv, err := Decode(platGroupAtMessage, json.RawMessage(groupMessagePayload))
	if err != nil {
		t.Fatalf("%s 解码失败: %v", platGroupAtMessage, err)
	}
	allEv, err := Decode(platGroupMessageAll, json.RawMessage(groupMessagePayload))
	if err != nil {
		t.Fatalf("%s 解码失败: %v", platGroupMessageAll, err)
	}
	// Name 记录平台原名，Mentioned 记录语义差异；其余字段必须完全一致。
	allEv.Name = atEv.Name
	allEv.Mentioned = atEv.Mentioned
	if !reflect.DeepEqual(atEv, allEv) {
		t.Fatalf("两种群消息事件除 Name/Mentioned 外应完全一致:\n@  %+v\n全量 %+v", atEv, allEv)
	}
}

// @ 的事实只能来自平台事件类型：官方文档对 mentions 的定义是
// 「消息中@的用户列表（不含@机器人自身）」，数组里永远不会有机器人自己。
// 因此 GROUP_AT_MESSAGE_CREATE 恒 true，其余消息事件恒 false——不猜。
func TestDecodeMentionedMarksOnlyGroupAtEvent(t *testing.T) {
	atEv, err := Decode(platGroupAtMessage, json.RawMessage(groupMessagePayload))
	if err != nil {
		t.Fatalf("%s 解码失败: %v", platGroupAtMessage, err)
	}
	if !atEv.Mentioned {
		t.Fatal("GROUP_AT_MESSAGE_CREATE 的 Mentioned 应为 true")
	}

	allEv, err := Decode(platGroupMessageAll, json.RawMessage(groupMessagePayload))
	if err != nil {
		t.Fatalf("%s 解码失败: %v", platGroupMessageAll, err)
	}
	// 全量模式的载荷里没有任何"@ 了机器人"的标记，只能保守为 false。
	if allEv.Mentioned {
		t.Fatal("GROUP_MESSAGE_CREATE 的 Mentioned 必须为 false（平台不标记，不做猜测）")
	}
}

// 单聊恒为 false：平台在单聊载荷里根本不标记 @，猜就是编造。
// 通知/未知事件同理——Mentioned 只描述消息。
func TestDecodeMentionedFalseOffMessagePaths(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		raw  string
	}{
		{"单聊消息", platC2CMessage, `{"id":"c1","content":"hi","author":{"id":"U1","user_openid":"U1"},"message_type":0}`},
		{"群通知", platGroupAddRobot, `{"group_openid":"G1","timestamp":1721500000}`},
		{"未知事件", "SOME_FUTURE_EVENT", `{"group_openid":"G1"}`},
	}
	for _, c := range cases {
		ev, err := Decode(c.typ, json.RawMessage(c.raw))
		if err != nil {
			t.Fatalf("%s 解码失败: %v", c.name, err)
		}
		if ev.Mentioned {
			t.Fatalf("%s 的 Mentioned 应为 false", c.name)
		}
	}
}

// mentions 里列的是"@ 了别人"，与"@ 了我"无关。这条测试把口径钉死：
// 即使载荷里带着非空 mentions，全量模式也仍是 Mentioned=false。
func TestDecodeMentionedIgnoresMentionsArray(t *testing.T) {
	ev, err := Decode(platGroupMessageAll, json.RawMessage(groupMessagePayload))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(ev.Raw) == 0 || !strings.Contains(string(ev.Raw), `"mentions"`) {
		t.Fatalf("前置条件：载荷里的 mentions 应原样进 Raw，实际 %s", ev.Raw)
	}
	if ev.Mentioned {
		t.Fatal("mentions 非空不代表 @ 了机器人：不得据此置位 Mentioned")
	}
}

// 已知事件也要能读到原始 JSON：平台加字段时插件可自救，不必等核心升契约。
func TestDecodeKeepsRawForKnownEvents(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		raw  string
	}{
		{"群消息", platGroupAtMessage, groupMessagePayload},
		{"单聊消息", platC2CMessage, `{"id":"c1","content":"hi","author":{"id":"U1","user_openid":"U1"},"message_type":0}`},
		{"群通知", platGroupAddRobot, `{"group_openid":"G1","timestamp":1721500000}`},
		{"未知事件", "SOME_FUTURE_EVENT", `{"group_openid":"G1","whatever":"x"}`},
	}
	for _, c := range cases {
		ev, err := Decode(c.typ, json.RawMessage(c.raw))
		if err != nil {
			t.Fatalf("%s 解码失败: %v", c.name, err)
		}
		if ev.RawTruncated {
			t.Fatalf("%s 的 Raw 不应被截断", c.name)
		}
		if string(ev.Raw) != c.raw {
			t.Fatalf("%s 的 Raw 应与原文逐字节相等:\n got %s\nwant %s", c.name, ev.Raw, c.raw)
		}
	}
}

// 超过上限时截断，但必须可判定：RawTruncated=true，且 Raw 是原文的严格前缀。
// 不允许"静默切出半截 JSON"——那样插件分不清是平台发了坏 JSON 还是本地切的。
func TestDecodeTruncatesRawAtLimit(t *testing.T) {
	big := `{"id":"M1","content":"` + strings.Repeat("a", host.MaxRawBytes) + `"}`
	ev, err := Decode(platGroupMessageAll, json.RawMessage(big))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if !ev.RawTruncated {
		t.Fatal("超长载荷的 RawTruncated 应为 true")
	}
	if len(ev.Raw) != host.MaxRawBytes {
		t.Fatalf("Raw 应截到 %d 字节，实际 %d", host.MaxRawBytes, len(ev.Raw))
	}
	if !bytes.HasPrefix([]byte(big), ev.Raw) {
		t.Fatal("截断后的 Raw 必须是原文的前缀")
	}
}

// 截断点不能落在多字节字符中间：Raw 若连合法 UTF-8 都不是，插件读到的
// 就是一段"平台发了坏编码"的假象，排查方向会被带偏。
// 上限不变（仍 ≤ MaxRawBytes），只是退到最近的 rune 边界。
func TestDecodeTruncatesRawAtRuneBoundary(t *testing.T) {
	big := `{"id":"M1","content":"` + strings.Repeat("汉", 11_000) + `"}`
	if utf8.Valid([]byte(big[:host.MaxRawBytes])) {
		t.Fatal("前置条件不成立：按字节硬切应正好落在汉字中间")
	}

	ev, err := Decode(platGroupMessageAll, json.RawMessage(big))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if !ev.RawTruncated {
		t.Fatal("超长载荷的 RawTruncated 应为 true")
	}
	if len(ev.Raw) > host.MaxRawBytes {
		t.Fatalf("Raw 不得超过 %d 字节，实际 %d", host.MaxRawBytes, len(ev.Raw))
	}
	if !utf8.Valid(ev.Raw) {
		t.Fatal("Raw 必须是合法 UTF-8：截断点退到了 rune 边界")
	}
	if !bytes.HasPrefix([]byte(big), ev.Raw) {
		t.Fatal("截断后的 Raw 必须是原文的前缀")
	}
	// 只丢一个残缺的 rune，不能借着"对齐"砍掉一截。
	if min := host.MaxRawBytes - utf8.UTFMax; len(ev.Raw) < min {
		t.Fatalf("Raw 应只退到最近的 rune 边界（≥ %d），实际 %d", min, len(ev.Raw))
	}
}

// 未截断时不得申请超过原文的内存：Raw 是副本而不是原文的切片，
// 这样事件生命周期结束后 RawMessage 所在的大 buffer 能被回收。
func TestDecodeRawIsIndependentCopy(t *testing.T) {
	raw := json.RawMessage(groupMessagePayload)
	ev, err := Decode(platGroupAtMessage, raw)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(ev.Raw) == 0 || len(raw) == 0 {
		t.Fatal("前置条件不成立")
	}
	if &ev.Raw[0] == &raw[0] {
		t.Fatal("Raw 必须是副本，不能与入参共享底层数组")
	}
	raw[0] = 'X'
	if ev.Raw[0] == 'X' {
		t.Fatal("修改入参不应影响 Raw")
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

// 内存实测用：单条真实群消息解码（含 Raw 副本）的分配量。
// 数字写进 ARCHITECTURE §1.3，换算过程见该节。
func BenchmarkDecodeGroupMessage(b *testing.B) {
	raw := json.RawMessage(groupMessagePayload)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for i := 0; i < b.N; i++ {
		if _, err := Decode(platGroupAtMessage, raw); err != nil {
			b.Fatal(err)
		}
	}
}

// 并发 16（dispatch 工作池默认上限）同时解码时的单条分配量。
// 每个 worker 同一时刻最多持有一条事件的 Raw，峰值 ≈ 16 × 单条 Raw 字节。
func BenchmarkDecodeGroupMessageConcurrent16(b *testing.B) {
	raw := json.RawMessage(groupMessagePayload)
	b.ReportAllocs()
	var wg sync.WaitGroup
	workers := 16
	per := b.N / workers
	if per == 0 {
		per = 1
	}
	b.ResetTimer()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := Decode(platGroupAtMessage, raw); err != nil {
					b.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// 最坏情况的分配上界：一条被截断的 32 KiB Raw。
func BenchmarkDecodeOversizeRaw(b *testing.B) {
	raw := json.RawMessage(`{"id":"M1","content":"` + strings.Repeat("a", host.MaxRawBytes) + `"}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Decode(platGroupMessageAll, raw); err != nil {
			b.Fatal(err)
		}
	}
}

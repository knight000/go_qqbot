package rpc

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// TestManifestRoundTrip：清单是握手的第一份数据，任何字段丢掉都表现为
// 「插件装上了但某些规则不生效」——最难查的那类。
func TestManifestRoundTrip(t *testing.T) {
	src := host.Manifest{
		Name:           "torn",
		Version:        "1.2.3",
		APIVersion:     host.APIVersion,
		Capabilities:   []string{"x"},
		Intents:        []string{"message", "group.notice"},
		Yield:          true,
		AllowedDomains: []string{"api.torn.com"},
		ConfigSchema:   map[string]string{"api_key": "string", "interval": "duration"},
		Permission:     host.PermGroupMember | host.PermBotAdmin,
		DenyReply:      "无权",
		Commands: []host.Command{{
			Prefix:      "/torn",
			Loose:       true,
			Pattern:     `^/t\s+`,
			Priority:    7,
			Description: "查战绩",
			Permission:  host.PermGroupOwner,
			DenyReply:   "仅群主",
		}},
	}
	data, err := json.Marshal(toManifestDTO(src))
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	var dto manifestDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		t.Fatalf("解码: %v", err)
	}
	got := dto.toHost()
	if !reflect.DeepEqual(got, src) {
		t.Fatalf("往返后不一致\n got = %#v\nwant = %#v", got, src)
	}
}

// TestEventRoundTrip 覆盖 Event 的每一个字段。Raw / Payload 是 []byte，
// 它们要能过 JSON（base64）而不损坏。
func TestEventRoundTrip(t *testing.T) {
	ts := time.Date(2026, 10, 23, 19, 0, 0, 0, time.UTC)
	src := &host.Event{
		ID:           "m1",
		Kind:         host.KindMessage,
		Name:         "GROUP_AT_MESSAGE_CREATE",
		Scene:        host.SceneGroup,
		Notice:       host.NoticeBotJoinedGroup,
		PeerID:       "g1",
		SenderID:     "u1",
		Username:     "昵称",
		UnionID:      "un1",
		MsgID:        "m1",
		Content:      "/md # 标题",
		Command:      "/md",
		Args:         "# 标题",
		MsgType:      0,
		HasMedia:     true,
		Mentioned:    true,
		Raw:          []byte(`{"raw":true}`),
		RawTruncated: true,
		Payload:      []byte(`{"payload":true}`),
		Timestamp:    ts,
		Actor: host.Principal{
			OpenID:    "u1",
			UnionID:   "un1",
			Scene:     host.SceneGroup,
			PeerID:    "g1",
			Role:      host.RoleAdmin,
			GroupRole: host.GroupRoleOwner,
		},
		Session: &host.SessionContext{Plugin: "torn", State: []byte(`{"step":2}`)},
	}
	data, err := json.Marshal(toEventDTO(src))
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	var dto eventDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		t.Fatalf("解码: %v", err)
	}
	got := dto.toHost()
	if !reflect.DeepEqual(got, src) {
		t.Fatalf("往返后不一致\n got = %#v\nwant = %#v", got, src)
	}
}

// TestEventNilSession：Session 的 nil 必须保持 nil——插件用它判断
// 「这条消息是不是我 Await 的回应」，无中生有一个零值会让每次静态路由
// 都被当成会话响应。
func TestEventNilSession(t *testing.T) {
	src := &host.Event{ID: "m1", Kind: host.KindMessage}
	var dto eventDTO
	data, _ := json.Marshal(toEventDTO(src))
	_ = json.Unmarshal(data, &dto)
	if got := dto.toHost(); got.Session != nil {
		t.Fatalf("Session = %+v，期望 nil", got.Session)
	}
}

// TestReplyRoundTrip 覆盖三种图片来源 + 零值。
func TestReplyRoundTrip(t *testing.T) {
	body := []byte{1, 2, 3, 0xff}
	cases := []struct {
		name     string
		in       *host.Reply
		wantBody []byte
	}{
		{"纯文本", &host.Reply{Content: "hi", MsgType: 2}, nil},
		{"零值", &host.Reply{}, nil},
		{"文件", &host.Reply{Image: host.ImageFromFile("a.png")}, nil},
		{"地址", &host.Reply{Image: host.ImageFromURL("https://x/y.png")}, nil},
		{"字节", &host.Reply{Content: "图", Image: host.ImageFromBytes(body)}, body},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dto, gotBody := encodeReply(c.in)
			if !reflect.DeepEqual(gotBody, c.wantBody) {
				t.Fatalf("正文 = %v，期望 %v", gotBody, c.wantBody)
			}
			data, err := json.Marshal(dto)
			if err != nil {
				t.Fatalf("编码: %v", err)
			}
			var back replyDTO
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatalf("解码: %v", err)
			}
			got, err := decodeReply(back, gotBody)
			if err != nil {
				t.Fatalf("还原: %v", err)
			}
			if !reflect.DeepEqual(got, c.in) {
				t.Fatalf("往返后不一致\n got = %#v\nwant = %#v", got, c.in)
			}
		})
	}
}

// TestJobSpecRoundTrip：Timeout 是 time.Duration，JSON 里是纳秒整数。
// 单位丢掉的表现是「任务 60 秒超时变成 60 纳秒」，看起来像插件立刻挂了。
func TestJobSpecRoundTrip(t *testing.T) {
	for _, in := range []host.JobSpec{
		{Plugin: "torn", Name: "tick", Cron: "@every 1m", Timeout: 60 * time.Second, Payload: `{"a":1}`, PerPeer: true, Peer: "g1"},
		{Plugin: "torn", Name: "tick"},
	} {
		var dto jobSpecDTO
		data, err := json.Marshal(toJobSpecDTO(in))
		if err != nil {
			t.Fatalf("编码: %v", err)
		}
		if err := json.Unmarshal(data, &dto); err != nil {
			t.Fatalf("解码: %v", err)
		}
		if got := dto.toHost(); got != in {
			t.Fatalf("往返后不一致\n got = %+v\nwant = %+v", got, in)
		}
	}
}

// TestNormalizeNums：TOML 的 int64 与 SQLite 的 int64 在 JSON 里都会变成
// 数字，收侧必须还原成 int64，否则 Params.Int 之外的读取路径全部走样。
func TestNormalizeNums(t *testing.T) {
	raw := json.RawMessage(`{"a":7,"b":1.5,"c":[1,2],"d":{"e":9},"f":"x","g":true}`)
	var m map[string]any
	if err := decodeJSON(raw, &m); err != nil {
		t.Fatalf("解码: %v", err)
	}
	norm := normalizeNums(m).(map[string]any)
	if v, ok := norm["a"].(int64); !ok || v != 7 {
		t.Fatalf("a = %#v，期望 int64(7)", norm["a"])
	}
	if v, ok := norm["b"].(float64); !ok || v != 1.5 {
		t.Fatalf("b = %#v，期望 float64(1.5)", norm["b"])
	}
	arr := norm["c"].([]any)
	if v, ok := arr[0].(int64); !ok || v != 1 {
		t.Fatalf("c[0] = %#v，期望 int64(1)", arr[0])
	}
	if v, ok := norm["d"].(map[string]any)["e"].(int64); !ok || v != 9 {
		t.Fatalf("d.e = %#v，期望 int64(9)", norm["d"])
	}
	if v, ok := norm["g"].(bool); !ok || !v {
		t.Fatalf("g = %#v，期望 true", norm["g"])
	}
}

// TestConfigIntFidelity 是「3.0 事故」的回归：配置里的整数经过一次
// JSON 往返后若变成 float64，Params.Decode 会把 `n = 7.0` 解进 int 字段
// 并报错——而插件作者只会看到「配置不合法」。
func TestConfigIntFidelity(t *testing.T) {
	raw := json.RawMessage(`{"greeting":"hi","n":7,"ratio":0.5}`)
	var m map[string]any
	if err := decodeJSON(raw, &m); err != nil {
		t.Fatalf("解码: %v", err)
	}
	p := host.NewParams("testplugin", normalizeNums(m).(map[string]any))
	if got := p.Int("n", -1); got != 7 {
		t.Fatalf("Params.Int = %d，期望 7", got)
	}
	var cfg struct {
		Greeting string
		N        int
		Ratio    float64
	}
	if err := p.Decode(&cfg); err != nil {
		t.Fatalf("Params.Decode: %v", err)
	}
	if cfg.Greeting != "hi" || cfg.N != 7 || cfg.Ratio != 0.5 {
		t.Fatalf("解出来 = %+v", cfg)
	}
}

// TestRowsIntFidelity：查询结果里的整数列必须是 int64。插件写
// `row["n"].(int64)` 是常规写法，float64 会让它 panic 或静默取零。
func TestRowsIntFidelity(t *testing.T) {
	raw := json.RawMessage(`[{"n":42,"s":"x"}]`)
	var rows []map[string]any
	if err := decodeJSON(raw, &rows); err != nil {
		t.Fatalf("解码: %v", err)
	}
	out := normalizeRows(rows)
	if v, ok := out[0]["n"].(int64); !ok || v != 42 {
		t.Fatalf("n = %#v，期望 int64(42)", out[0]["n"])
	}
	if normalizeRows(nil) != nil {
		t.Fatal("nil 行集应保持 nil（插件靠它判「没有数据」）")
	}
}

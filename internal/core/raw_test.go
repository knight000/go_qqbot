package core

import (
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// Raw 只在 dispatch 生命周期内有效：不落库、不参与去重键。
// 这条测试守的是"别顺手把 Raw 写进 messages"——落库路径是显式字段拼装，
// 加字段的人很容易顺手带上。
func TestRawIsNotPersisted(t *testing.T) {
	c, _, _, ctx := newGateCore(t)
	// 放行该群，让事件真正走到落库与路由（默认白名单会先把它丢掉）。
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeAllow); err != nil {
		t.Fatalf("设置群模式: %v", err)
	}

	const marker = "RAW_ONLY_MARKER"
	raw := `{"id":"m-raw","extra":"` + marker + `"}`
	ev := &host.Event{
		ID: "e-raw", Kind: host.KindMessage, Name: "GROUP_AT_MESSAGE_CREATE",
		Scene: host.SceneGroup, PeerID: "g1", SenderID: "u1",
		MsgID: "m-raw", Content: "hi", Mentioned: true,
		Timestamp: time.Now(),
		Raw:       []byte(raw),
	}
	c.Handle(ctx, ev)

	rows, err := c.store.QueryRows(ctx, "SELECT * FROM messages")
	if err != nil {
		t.Fatalf("查询消息: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应落库 1 条消息，实际 %d", len(rows))
	}
	for col, v := range rows[0] {
		if s, ok := v.(string); ok && strings.Contains(s, marker) {
			t.Fatalf("messages.%s 不该出现 Raw 的内容: %q", col, s)
		}
	}

	// 去重键是事件 ID：同 ID、不同 Raw 的第二次投递必须被拦下，
	// 否则 Raw 就等于偷偷参与了幂等判定。
	second := *ev
	second.Raw = []byte(`{"id":"m-raw","extra":"different"}`)
	c.Handle(ctx, &second)

	if n := countRows(t, c.store, "messages"); n != 1 {
		t.Fatalf("重复投递不应新增消息行，实际 %d", n)
	}
	if n := countRows(t, c.store, "processed_events"); n != 1 {
		t.Fatalf("同 ID 只应占一行去重记录，实际 %d", n)
	}

	// Handle 不得改写 Raw：它是只读事实，生命周期由持有者决定。
	if string(ev.Raw) != raw {
		t.Fatalf("Handle 不应改写 Raw，实际 %s", ev.Raw)
	}
}

// 群准入丢弃路径走的是同一份事件对象：它不落库、不占去重表，
// 也不应顺手把 Raw 清掉"省内存"——生命周期由 dispatch 决定。
func TestRawSurvivesDroppedPaths(t *testing.T) {
	c, _, _, ctx := newGateCore(t)

	raw := []byte(`{"id":"m-x"}`)
	ev := &host.Event{
		ID: "e-drop", Kind: host.KindMessage, Name: "GROUP_MESSAGE_CREATE",
		Scene: host.SceneGroup, PeerID: "g-unauthorized", SenderID: "u1",
		MsgID: "m-x", Content: "hi", Timestamp: time.Now(), Raw: raw,
	}
	c.Handle(ctx, ev) // 未放行的群 → 在去重之前就被丢弃
	if string(ev.Raw) != string(raw) {
		t.Fatalf("丢弃路径不应改写 Raw，实际 %s", ev.Raw)
	}
	if n := countRows(t, c.store, "processed_events"); n != 0 {
		t.Fatalf("被丢弃的事件不应占去重表，实际 %d", n)
	}
	if n := countRows(t, c.store, "messages"); n != 0 {
		t.Fatalf("被丢弃的事件不应落库，实际 %d 行", n)
	}
}

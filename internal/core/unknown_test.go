package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

type countingPlugin struct {
	fakePlugin
	hits int
}

func (p *countingPlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	p.hits++
	return nil, nil
}

// 未知事件只观测：不路由、不落库、不占去重表。
// 这是 ADR-0001「未知事件永不进入插件路由」的守门测试。
func TestUnknownEventNeverRoutesOrStores(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "unknown.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("迁移: %v", err)
	}

	log := slog.New(slog.DiscardHandler)
	// 订阅全部消息、且不声明命令的插件：凡是"能路由的"都会命中它。
	plug := &countingPlugin{fakePlugin: fakePlugin{m: host.Manifest{
		Name:       "spy",
		APIVersion: host.APIVersion,
		Intents:    []string{"message"},
	}}}
	router, err := NewRouter([]host.Plugin{plug})
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	out := &syncBuffer{}
	tr := NewTrace(out, LevelFull)
	tr.SetUnknownPayload(true)
	disp := NewDispatcher(router, NewSender(&fakeAPI{}, log, ActivePolicy{}), log, time.Second)
	disp.trace = tr

	c := &Core{store: db, log: log, trace: tr, router: router, dispatch: disp}
	c.Handle(ctx, &host.Event{
		Kind:      host.KindUnknown,
		Name:      "SOME_FUTURE_EVENT",
		Payload:   []byte(`{"group_openid":"G1"}`),
		Timestamp: time.Now(),
	})

	if plug.hits != 0 {
		t.Fatalf("未知事件不应进入插件路由，实际命中 %d 次", plug.hits)
	}
	got := out.String()
	for _, want := range []string{"未知事件 SOME_FUTURE_EVENT", `{"group_openid":"G1"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("轨迹缺少 %q，实际输出:\n%s", want, got)
		}
	}

	if n := countRows(t, db, "messages"); n != 0 {
		t.Fatalf("未知事件不应落库，messages 行数=%d", n)
	}
	if n := countRows(t, db, "processed_events"); n != 0 {
		t.Fatalf("未知事件不应占用去重表，processed_events 行数=%d", n)
	}
}

// 消息事件仍然会落库，并且写入平台真实的 message_type（此前被写死为 0）。
func TestMessageStoresPlatformMsgType(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "msgtype.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("迁移: %v", err)
	}

	log := slog.New(slog.DiscardHandler)
	router, err := NewRouter(nil)
	if err != nil {
		t.Fatalf("装配空路由: %v", err)
	}
	disp := NewDispatcher(router, NewSender(&fakeAPI{}, log, ActivePolicy{}), log, time.Second)
	c := &Core{store: db, log: log, router: router, dispatch: disp}

	c.Handle(ctx, &host.Event{
		ID: "e-1", Kind: host.KindMessage, Name: "GROUP_MESSAGE_CREATE",
		Scene: host.SceneGroup, PeerID: "G1", SenderID: "M1",
		MsgID: "m1", Content: "hi", MsgType: 3, Timestamp: time.Now(),
	})

	rows, err := db.QueryRows(ctx, "SELECT msg_type, scene FROM messages")
	if err != nil {
		t.Fatalf("查询消息: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应落库 1 条消息，实际 %d", len(rows))
	}
	if got := rows[0]["msg_type"]; got != int64(3) {
		t.Fatalf("msg_type 应为平台真值 3，实际 %v(%T)", got, got)
	}
	if got := rows[0]["scene"]; got != "group" {
		t.Fatalf("scene 应为 group，实际 %v", got)
	}
}

func countRows(t *testing.T, db *store.DB, table string) int {
	t.Helper()
	rows, err := db.QueryRows(context.Background(), "SELECT COUNT(*) AS n FROM "+table)
	if err != nil {
		t.Fatalf("统计 %s: %v", table, err)
	}
	if len(rows) != 1 {
		t.Fatalf("统计 %s 返回 %d 行", table, len(rows))
	}
	switch v := rows[0]["n"].(type) {
	case int64:
		return int(v)
	case int:
		return v
	case float64:
		return int(v)
	}
	t.Fatalf("无法识别的计数类型 %T", rows[0]["n"])
	return 0
}

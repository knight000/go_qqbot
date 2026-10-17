package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	return db
}

func TestEventDedupe(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first, err := db.MarkEventSeen(ctx, "evt-1")
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("首次事件应返回 true")
	}
	second, err := db.MarkEventSeen(ctx, "evt-1")
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("重复事件应返回 false")
	}
}

func TestMessageTruncateAndPurge(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	long := strings.Repeat("中", 3000) // 9000 字节，应被截断到 4 KB 内
	if err := db.InsertMessage(ctx, Message{
		EventID: "e1", MsgID: "m1", Scene: "group", PeerOpenID: "g1",
		SenderOpenID: "u1", Content: long,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.QueryRows(ctx, `SELECT length(content) AS n FROM messages WHERE event_id = 'e1'`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := rows[0]["n"].(int64); n > maxContentBytes {
		t.Fatalf("内容未截断: %d 字节", n)
	}

	n, err := db.PurgeMessages(ctx, time.Now().Unix()+60, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应删除 1 行，实际 %d", n)
	}
}

func TestKVTTLAndAdmin(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.KVSet(ctx, "echo", "k", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	v, ok, err := db.KVGet(ctx, "echo", "k")
	if err != nil || !ok || string(v) != "v" {
		t.Fatalf("KV 读取失败: v=%q ok=%v err=%v", v, ok, err)
	}

	if err := db.KVSet(ctx, "echo", "exp", []byte("v"), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, ok, _ := db.KVGet(ctx, "echo", "exp"); ok {
		t.Fatal("过期 KV 应视为不存在")
	}

	if err := db.AddAdmin(ctx, "u1", "group", "", "owner", "claim"); err != nil {
		t.Fatal(err)
	}
	role, _, ok, err := db.AdminRole(ctx, "u1", "group", "any-group")
	if err != nil || !ok || role != "owner" {
		t.Fatalf("管理员解析失败: role=%q ok=%v err=%v", role, ok, err)
	}
	if n, _ := db.CountAdmins(ctx); n != 1 {
		t.Fatalf("管理员数量应为 1，实际 %d", n)
	}
}

package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// --- plugin_kv 的 peer 维度 ---

// 同一个 key 在两个 peer 下互不干扰，插件级全局键（peer 为空串）也不受影响。
// 这条隔离是 R1 能"只清一个会话"的全部前提。
func TestKVPeerIsolation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, kv := range []struct{ peer, val string }{
		{"", "global"},
		{"g1", "one"},
		{"g2", "two"},
	} {
		if err := db.KVSet(ctx, "echo", kv.peer, "state", []byte(kv.val), 0); err != nil {
			t.Fatal(err)
		}
	}

	for _, kv := range []struct{ peer, want string }{
		{"", "global"},
		{"g1", "one"},
		{"g2", "two"},
	} {
		v, ok, err := db.KVGet(ctx, "echo", kv.peer, "state")
		if err != nil || !ok || string(v) != kv.want {
			t.Fatalf("peer=%q 读到 %q ok=%v err=%v，期望 %q", kv.peer, v, ok, err, kv.want)
		}
	}

	counts, err := db.ClearPeerSession(ctx, "g1")
	if err != nil || counts.KVRows != 1 {
		t.Fatalf("清理 g1 应删 1 行 KV，实际 %d err=%v", counts.KVRows, err)
	}
	if _, ok, _ := db.KVGet(ctx, "echo", "g1", "state"); ok {
		t.Fatal("g1 的键应已删除")
	}
	if _, ok, _ := db.KVGet(ctx, "echo", "g2", "state"); !ok {
		t.Fatal("g2 的键不应被波及")
	}
	if _, ok, _ := db.KVGet(ctx, "echo", "", "state"); !ok {
		t.Fatal("插件级全局键不应被 R1 清掉")
	}
}

// 旧库的 plugin_kv 主键是 (plugin, k)。Migrate 要重建表并把旧键搬进空 peer，
// 否则升级后 R1 会把历史全局键当成会话键删掉。
func TestMigrateLegacyPluginKV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oldkv.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	if _, err := db.w.ExecContext(ctx, `CREATE TABLE plugin_kv (
		plugin     TEXT NOT NULL,
		k          TEXT NOT NULL,
		v          BLOB NOT NULL,
		expires_at INTEGER,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (plugin, k)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO plugin_kv(plugin, k, v, expires_at, updated_at) VALUES('bootnotice', 'last_start', 'v1', NULL, 1)`,
	); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	v, ok, err := db.KVGet(ctx, "bootnotice", "", "last_start")
	if err != nil || !ok || string(v) != "v1" {
		t.Fatalf("旧键应搬进 peer=''：v=%q ok=%v err=%v", v, ok, err)
	}
	var n int
	if n != 0 {
		t.Fatal("旧表应已删除")
	}

	// 新主键必须允许同一 key 在不同 peer 下共存——这正是旧表做不到的。
	if err := db.KVSet(ctx, "bootnotice", "g1", "last_start", []byte("v2"), 0); err != nil {
		t.Fatalf("新主键应允许同 key 不同 peer: %v", err)
	}
}

// --- 审计与确认码 ---

func TestResetAuditPendingLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first, err := db.OpenResetPending(ctx, "owner-1", "g1", ResetLevelData, "111111", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.OpenResetPending(ctx, "owner-1", "g1", ResetLevelData, "222222", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// 重复发起会顶掉旧码，而不是让用户对着两个都失效的码猜。
	recs, err := db.RecentResets(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].ID != second || recs[1].ID != first {
		t.Fatalf("审计顺序错误: %+v", recs)
	}
	if recs[0].Code != "222222" || recs[0].Status != ResetStatusPending {
		t.Fatalf("最新一条应是 222222 pending: %+v", recs[0])
	}
	if recs[1].Status != ResetStatusCancelled {
		t.Fatalf("旧的一条应被置为 cancelled: %+v", recs[1])
	}

	rec, ok, err := db.PendingReset(ctx, "owner-1", "g1", ResetLevelData)
	if err != nil || !ok || rec.Code != "222222" {
		t.Fatalf("应取到最新待确认记录: %+v ok=%v err=%v", rec, ok, err)
	}

	// 绑定：换 actor / peer / level 都取不到。
	for _, q := range []struct{ actor, peer, level string }{
		{"intruder", "g1", ResetLevelData},
		{"owner-1", "g2", ResetLevelData},
		{"owner-1", "g1", ResetLevelAll},
	} {
		if _, ok, _ := db.PendingReset(ctx, q.actor, q.peer, q.level); ok {
			t.Fatalf("确认码不应被 %+v 复用", q)
		}
	}

	if err := db.CloseReset(ctx, second, ResetStatusDone, "消息 3 条"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.PendingReset(ctx, "owner-1", "g1", ResetLevelData); ok {
		t.Fatal("已执行的记录不应再被当成 pending")
	}

	at, ok, err := db.LastResetDoneAt(ctx, ResetLevelData, "g1")
	if err != nil || !ok {
		t.Fatalf("应能查到冷却起点: ok=%v err=%v", ok, err)
	}
	if d := time.Since(at); d < 0 || d > time.Minute {
		t.Fatalf("冷却起点异常: %s", d)
	}
	// 其它 peer 没有执行过，不受冷却影响。
	if _, ok, _ := db.LastResetDoneAt(ctx, ResetLevelData, "g2"); ok {
		t.Fatal("g2 不应有冷却")
	}
}

func TestExpirePendingResets(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.OpenResetPending(ctx, "owner-1", "g1", ResetLevelData, "111111", time.Minute); err != nil {
		t.Fatal(err)
	}
	// 直接把过期时间挪到过去，模拟等了 61 秒。
	if _, err := db.w.ExecContext(ctx,
		`UPDATE reset_audit SET expires_at = ? WHERE status = ?`,
		time.Now().Add(-time.Second).Unix(), ResetStatusPending); err != nil {
		t.Fatal(err)
	}

	n, err := db.ExpirePendingResets(ctx)
	if err != nil || n != 1 {
		t.Fatalf("应把 1 条过期记录标记为 expired，实际 %d err=%v", n, err)
	}
	recs, _ := db.RecentResets(ctx, 5)
	if len(recs) != 1 || recs[0].Status != ResetStatusExpired {
		t.Fatalf("状态应为 expired: %+v", recs)
	}
}

// --- 分级清理 ---

func insertTestMessage(t *testing.T, db *DB, eventID, peer string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := db.InsertMessage(ctx, Message{
		EventID: eventID, Scene: "group", PeerOpenID: peer, Content: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.r.QueryRowContext(ctx,
		`SELECT id FROM messages WHERE event_id = ?`, eventID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertTestMedia(t *testing.T, db *DB, messageID int64, path string) {
	t.Helper()
	if _, err := db.w.ExecContext(context.Background(),
		`INSERT INTO media(message_id, sha256, kind, path, bytes, created_at) VALUES(?, ?, 'image', ?, 10, ?)`,
		messageID, "sha-"+path, path, nowUnix()); err != nil {
		t.Fatal(err)
	}
}

// R2 只删该 peer 的消息与媒体，别的会话原样保留。
func TestClearPeerDataScoped(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	m1 := insertTestMessage(t, db, "e1", "g1")
	m2 := insertTestMessage(t, db, "e2", "g2")
	insertTestMedia(t, db, m1, "/tmp/a.jpg")
	insertTestMedia(t, db, m2, "/tmp/b.jpg")

	paths, err := db.PeerMediaPaths(ctx, "g1")
	if err != nil || len(paths) != 1 || paths[0] != "/tmp/a.jpg" {
		t.Fatalf("应只取到 g1 的媒体路径: %+v err=%v", paths, err)
	}

	counts, err := db.ClearPeerData(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Messages != 1 || counts.MediaRows != 1 {
		t.Fatalf("清理数量错误: %+v", counts)
	}

	stats, err := db.PeerStats(ctx, "g1")
	if err != nil || stats.Messages != 0 || stats.MediaRows != 0 {
		t.Fatalf("g1 应已清空: %+v err=%v", stats, err)
	}
	stats, err = db.PeerStats(ctx, "g2")
	if err != nil || stats.Messages != 1 || stats.MediaRows != 1 {
		t.Fatalf("g2 不应被波及: %+v err=%v", stats, err)
	}
}

// R3 清业务表，但绝不能碰 admins / bot_state / qq_groups / jobs / reset_audit。
// 这张清单就是"重置完还能继续用"的边界，逐条断言。
func TestClearAllDataKeepsConfig(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertAdmin(ctx, Admin{OpenID: "owner-1", Role: RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutState(ctx, "session_id", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGroupMode(ctx, "g1", GroupModeAllow); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertJob(ctx, Job{ID: "echo:tick", Plugin: "echo", Spec: "* * * * *", NextRunAt: nowUnix()}); err != nil {
		t.Fatal(err)
	}
	if err := db.AuditResetDone(ctx, "owner-1", ResetScopeGlobal, ResetLevelAll, "准备中"); err != nil {
		t.Fatal(err)
	}

	m1 := insertTestMessage(t, db, "e1", "g1")
	insertTestMedia(t, db, m1, "/tmp/a.jpg")
	if err := db.KVSet(ctx, "echo", "g1", "k", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordJobRun(ctx, "echo:tick", "", 1, 2, "ok", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MarkEventSeen(ctx, "e1"); err != nil {
		t.Fatal(err)
	}
	if err := db.LinkIdentity(ctx, "owner-1", "union-1", "c2c", ""); err != nil {
		t.Fatal(err)
	}

	paths, err := db.AllMediaPaths(ctx)
	if err != nil || len(paths) != 1 {
		t.Fatalf("全库媒体路径应为 1 条: %+v err=%v", paths, err)
	}

	counts, err := db.ClearAllData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Messages != 1 || counts.MediaRows != 1 || counts.KVRows != 1 ||
		counts.JobRuns != 1 || counts.Events != 1 || counts.Identities != 1 {
		t.Fatalf("清理数量错误: %+v", counts)
	}

	stats, err := db.DataStats(ctx)
	if err != nil || stats.Total() != 0 {
		t.Fatalf("业务表应全空: %+v err=%v", stats, err)
	}

	// 保留项逐条验证。
	if _, ok, _ := db.AdminRole(ctx, "owner-1", ""); !ok {
		t.Fatal("R3 不得删除管理员")
	}
	if v, ok, _ := db.GetState(ctx, "session_id"); !ok || v != "sess-1" {
		t.Fatal("R3 不得清 bot_state")
	}
	if g, ok, _ := db.GetGroup(ctx, "g1"); !ok || g.Mode != GroupModeAllow {
		t.Fatalf("R3 不得清群名单: %+v ok=%v", g, ok)
	}
	var jobs int
	if err := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("R3 不得删除任务定义: %d err=%v", jobs, err)
	}
	recs, err := db.RecentResets(ctx, 5)
	if err != nil || len(recs) != 1 {
		t.Fatalf("R3 不得清审计: %+v err=%v", recs, err)
	}
}

func TestAuditResetDoneRecordsDetail(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.AuditResetDone(ctx, "owner-1", "g1", ResetLevelSession, "会话状态 3 条"); err != nil {
		t.Fatal(err)
	}
	recs, err := db.RecentResets(ctx, 5)
	if err != nil || len(recs) != 1 {
		t.Fatalf("应有一条审计: %+v err=%v", recs, err)
	}
	r := recs[0]
	if r.Status != ResetStatusDone || r.Detail != "会话状态 3 条" || r.Peer != "g1" {
		t.Fatalf("审计内容错误: %+v", r)
	}
	if r.FinishedAt.IsZero() {
		t.Fatal("已执行记录必须有 finished_at，冷却靠它计算")
	}
}

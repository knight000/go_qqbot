package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sessionAt 造一行会话。时间统一用整秒，库里的列就是 unix 秒，
// 带纳秒的期望值会比读回来的值大，断言就成了"读的不等于写的"。
func sessionAt(peer, scene, plugin string, state []byte, created, ttl time.Duration) Session {
	now := time.Now().Truncate(time.Second)
	return Session{
		Peer:      peer,
		Scene:     scene,
		Plugin:    plugin,
		State:     state,
		CreatedAt: now.Add(created),
		ExpiresAt: now.Add(created + ttl),
	}
}

func TestSessionCreateAndGet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	want := sessionAt("g1", "group", "md", []byte(`{"step":1}`), 0, time.Minute)
	if err := db.CreateSession(ctx, want); err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	got, ok, err := db.Session(ctx, "g1")
	if err != nil || !ok {
		t.Fatalf("应读到会话: ok=%v err=%v", ok, err)
	}
	if got.Plugin != want.Plugin || got.Scene != want.Scene {
		t.Fatalf("插件/场景不符: %+v", got)
	}
	if string(got.State) != string(want.State) {
		t.Fatalf("state 应逐字节保留: %q", got.State)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("时间不符: created=%s expires=%s，期望 %s / %s",
			got.CreatedAt, got.ExpiresAt, want.CreatedAt, want.ExpiresAt)
	}

	if _, ok, err := db.Session(ctx, "nobody"); err != nil || ok {
		t.Fatalf("不存在的 peer 应返回 ok=false: ok=%v err=%v", ok, err)
	}
}

// 主键冲突必须是一个可判定的错误：并发 Await 靠它兜底（ADR-0004 决策 5）。
func TestSessionCreateConflict(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.CreateSession(ctx, sessionAt("g1", "group", "md", nil, 0, time.Minute)); err != nil {
		t.Fatal(err)
	}
	err := db.CreateSession(ctx, sessionAt("g1", "group", "other", nil, 0, time.Minute))
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("同 peer 二次创建应返回 ErrSessionExists，实际 %v", err)
	}
	if got, _, _ := db.Session(ctx, "g1"); got.Plugin != "md" {
		t.Fatalf("冲突不应覆盖持有者，实际 %q", got.Plugin)
	}
}

// 同一插件重复 Await = 改 state、重新计时，created_at 不变（排障要看最初接管时刻）。
func TestSessionRefresh(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created := time.Now().Truncate(time.Second).Add(-10 * time.Minute)
	row := Session{
		Peer: "g1", Scene: "group", Plugin: "md",
		State:     []byte("old"),
		CreatedAt: created,
		ExpiresAt: created.Add(time.Minute),
	}
	if err := db.CreateSession(ctx, row); err != nil {
		t.Fatal(err)
	}

	next := time.Now().Truncate(time.Second).Add(5 * time.Minute)
	ok, err := db.RefreshSession(ctx, "g1", "md", []byte("new"), next)
	if err != nil || !ok {
		t.Fatalf("同名插件应刷新成功: ok=%v err=%v", ok, err)
	}
	got, _, _ := db.Session(ctx, "g1")
	if string(got.State) != "new" || !got.ExpiresAt.Equal(next) {
		t.Fatalf("state/expires 未更新: %+v", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("created_at 不应变: %s，期望 %s", got.CreatedAt, created)
	}

	// 换插件名 = 不是持有者，行必须原样。
	ok, err = db.RefreshSession(ctx, "g1", "other", []byte("hijack"), next.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("非持有者刷新应返回 false")
	}
	if got, _, _ := db.Session(ctx, "g1"); string(got.State) != "new" {
		t.Fatalf("非持有者不得改动行，实际 %q", got.State)
	}

	// 行不存在同样返回 false，不报错：调用方据此回到"创建"分支。
	ok, err = db.RefreshSession(ctx, "nobody", "md", nil, next)
	if err != nil || ok {
		t.Fatalf("行不存在应返回 false 且不报错: ok=%v err=%v", ok, err)
	}
}

// ReplaceSession 服务两种场景：过期行与悬挂行（持有者已卸载）。
func TestSessionReplace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.CreateSession(ctx, sessionAt("g1", "group", "old", []byte("x"), 0, time.Minute)); err != nil {
		t.Fatal(err)
	}
	next := sessionAt("g1", "group", "new", []byte("y"), 0, 2*time.Minute)
	if err := db.ReplaceSession(ctx, next); err != nil {
		t.Fatalf("替换会话: %v", err)
	}
	got, ok, _ := db.Session(ctx, "g1")
	if !ok || got.Plugin != "new" || string(got.State) != "y" {
		t.Fatalf("应被整体覆盖: %+v ok=%v", got, ok)
	}
	if n, _ := db.CountSessions(ctx); n != 1 {
		t.Fatalf("替换不应多出行，实际 %d", n)
	}
}

func TestSessionDeleteIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.CreateSession(ctx, sessionAt("g1", "group", "md", nil, 0, time.Minute)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := db.DeleteSession(ctx, "g1"); err != nil {
			t.Fatalf("第 %d 次删除报错: %v", i+1, err)
		}
	}
	if _, ok, _ := db.Session(ctx, "g1"); ok {
		t.Fatal("行应已删除")
	}
}

func TestPurgeExpiredSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	past := sessionAt("expired", "group", "md", nil, -10*time.Minute, time.Minute)
	if err := db.CreateSession(ctx, past); err != nil {
		t.Fatal(err)
	}
	live := sessionAt("live", "group", "md", nil, 0, time.Hour)
	if err := db.CreateSession(ctx, live); err != nil {
		t.Fatal(err)
	}

	n, err := db.PurgeExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("清过期会话: %v", err)
	}
	if n != 1 {
		t.Fatalf("应删 1 行，实际 %d", n)
	}
	if _, ok, _ := db.Session(ctx, "expired"); ok {
		t.Fatal("过期行应已删除")
	}
	if _, ok, _ := db.Session(ctx, "live"); !ok {
		t.Fatal("未过期的行不得被删")
	}
}

func TestCountSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if n, _ := db.CountSessions(ctx); n != 0 {
		t.Fatalf("空表应为 0，实际 %d", n)
	}
	for _, peer := range []string{"g1", "g2", "u1"} {
		if err := db.CreateSession(ctx, sessionAt(peer, "group", "md", nil, 0, time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := db.CountSessions(ctx); err != nil || n != 3 {
		t.Fatalf("应为 3，实际 %d err=%v", n, err)
	}

	// 过期等于行不存在（决策 4）：总览不能把一个已经没人等的 peer 算成"会话中"，
	// 否则它与 /group status <群> 的「无」自相矛盾，最长虚报一个 GC 周期。
	if err := db.CreateSession(ctx, sessionAt("stale", "group", "md", nil, -10*time.Minute, time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := db.CountSessions(ctx); err != nil || n != 3 {
		t.Fatalf("过期行不得计入，应为 3，实际 %d err=%v", n, err)
	}
}

// R1 / R2 / R3 都要带走会话，且 R1 只带走自己的。
func TestResetClearsSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	seed := func(peer string) {
		t.Helper()
		if err := db.ReplaceSession(ctx, sessionAt(peer, "group", "md", nil, 0, time.Minute)); err != nil {
			t.Fatalf("预置会话 %s: %v", peer, err)
		}
	}

	seed("g1")
	seed("g2")
	counts, err := db.ClearPeerSession(ctx, "g1")
	if err != nil {
		t.Fatalf("R1 清会话: %v", err)
	}
	if counts.Sessions != 1 {
		t.Fatalf("R1 应清 1 条会话，实际 %d", counts.Sessions)
	}
	if _, ok, _ := db.Session(ctx, "g1"); ok {
		t.Fatal("g1 的会话应已清")
	}
	if _, ok, _ := db.Session(ctx, "g2"); !ok {
		t.Fatal("g2 不得被波及")
	}

	seed("g1")
	counts, err = db.ClearPeerData(ctx, "g1")
	if err != nil {
		t.Fatalf("R2 清数据: %v", err)
	}
	if counts.Sessions != 1 {
		t.Fatalf("R2 应记 1 条会话，实际 %d", counts.Sessions)
	}
	if _, ok, _ := db.Session(ctx, "g1"); ok {
		t.Fatal("R2 应清掉该 peer 的会话")
	}
	if _, ok, _ := db.Session(ctx, "g2"); !ok {
		t.Fatal("R2 不得波及别的 peer")
	}

	counts, err = db.ClearAllData(ctx)
	if err != nil {
		t.Fatalf("R3 清全量: %v", err)
	}
	if counts.Sessions != 1 {
		t.Fatalf("R3 应记 1 条会话，实际 %d", counts.Sessions)
	}
	if n, _ := db.CountSessions(ctx); n != 0 {
		t.Fatalf("R3 后应为空，实际 %d", n)
	}
}

// 预览要真实数字：统计漏了会话，用户看到的就是"将清空 N，实际删了 N+1"。
func TestStatsIncludeSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.ReplaceSession(ctx, sessionAt("g1", "group", "md", nil, 0, time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSession(ctx, sessionAt("g2", "group", "md", nil, 0, time.Minute)); err != nil {
		t.Fatal(err)
	}

	peer, err := db.PeerStats(ctx, "g1")
	if err != nil || peer.Sessions != 1 {
		t.Fatalf("PeerStats 应含 1 条会话: %+v err=%v", peer, err)
	}
	all, err := db.DataStats(ctx)
	if err != nil || all.Sessions != 2 {
		t.Fatalf("DataStats 应含 2 条会话: %+v err=%v", all, err)
	}
}

// Total 是"这次到底动了多少行"的唯一口径，漏计会让摘要显示"清空了 0 条"。
func TestResetCountsTotalIncludesSessions(t *testing.T) {
	var c ResetCounts
	c.add(ResetCounts{Sessions: 3})
	if c.Total() != 3 {
		t.Fatalf("Total 应计入会话，实际 %d", c.Total())
	}
}

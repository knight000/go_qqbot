package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTakeActiveBudgetStopsAtLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		ok, err := db.TakeActiveBudget(ctx, ActiveUsageKey{Day: "2026-10-20", Scene: "c2c", Peer: "u1"}, 2)
		if err != nil {
			t.Fatalf("第 %d 次占额度出错: %v", i, err)
		}
		if !ok {
			t.Fatalf("第 %d 次应占额度成功", i)
		}
	}
	ok, err := db.TakeActiveBudget(ctx, ActiveUsageKey{Day: "2026-10-20", Scene: "c2c", Peer: "u1"}, 2)
	if err != nil {
		t.Fatalf("超额占额度出错: %v", err)
	}
	if ok {
		t.Fatal("limit=2 时第 3 次必须失败")
	}

	// 维度是 (day, scene, peer)：换一天、换场景、换 peer 都不受影响。
	for _, k := range []ActiveUsageKey{
		{Day: "2026-10-21", Scene: "c2c", Peer: "u1"},
		{Day: "2026-10-20", Scene: "group", Peer: "u1"},
		{Day: "2026-10-20", Scene: "c2c", Peer: "u2"},
	} {
		got, err := db.TakeActiveBudget(ctx, k, 2)
		if err != nil || !got {
			t.Fatalf("独立维度 %v 应可占额度: ok=%v err=%v", k, got, err)
		}
	}
}

// limit <= 0 必须直接拒发，连行都不该写。
// 单靠 WHERE used < ? 拦不住：upsert 的 INSERT 分支不看 WHERE，
// 新 key 会插进一行 used=1 并返回成功——那等于把"不限量"错读成"放开"。
func TestTakeActiveBudgetNonPositiveLimitRefuses(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, limit := range []int{0, -1} {
		ok, err := db.TakeActiveBudget(ctx, ActiveUsageKey{Day: "2026-10-20", Scene: "c2c", Peer: "u1"}, limit)
		if err != nil {
			t.Fatalf("limit=%d 不该报错: %v", limit, err)
		}
		if ok {
			t.Fatalf("limit=%d 必须拒发", limit)
		}
	}
	var n int
	if err := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM active_usage`).Scan(&n); err != nil {
		t.Fatalf("统计预算行: %v", err)
	}
	if n != 0 {
		t.Fatalf("limit<=0 时不该写任何预算行，实际 %d 行", n)
	}
}

// 落库的全部意义：换一个连接重开同一个库，额度还在。
func TestActiveBudgetSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	ctx := context.Background()

	open := func() *DB {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("打开测试库: %v", err)
		}
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("迁移: %v", err)
		}
		return db
	}

	db := open()
	if ok, err := db.TakeActiveBudget(ctx, ActiveUsageKey{Day: "2026-10-20", Scene: "group", Peer: "g1"}, 1); err != nil || !ok {
		t.Fatalf("首次应可占额度: ok=%v err=%v", ok, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关库: %v", err)
	}

	db2 := open()
	defer db2.Close()
	if ok, err := db2.TakeActiveBudget(ctx, ActiveUsageKey{Day: "2026-10-20", Scene: "group", Peer: "g1"}, 1); err != nil || ok {
		t.Fatalf("重开后额度应已用尽: ok=%v err=%v", ok, err)
	}
}

// 预算属于业务数据：R2 清该 peer、R3 清全表；R1（会话状态）与 R0（重连）不动它。
// 断言一律用"再占一格能不能占到"——这正是 Sender 看到的那个语义。
func TestResetLevelsAndActiveBudget(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const day = "2026-10-20"

	take := func(peer string) bool {
		t.Helper()
		ok, err := db.TakeActiveBudget(ctx, ActiveUsageKey{Day: day, Scene: "group", Peer: peer}, 1)
		if err != nil {
			t.Fatalf("占 %s 的额度: %v", peer, err)
		}
		return ok
	}
	if !take("g1") || !take("g2") {
		t.Fatal("两个 peer 的额度都该占到第一次")
	}

	if _, err := db.ClearPeerSession(ctx, "g1"); err != nil {
		t.Fatalf("R1: %v", err)
	}
	if take("g1") {
		t.Fatal("R1 不该清空主动消息预算")
	}

	if _, err := db.ClearPeerData(ctx, "g2"); err != nil {
		t.Fatalf("R2: %v", err)
	}
	if !take("g2") {
		t.Fatal("R2 应清掉该 peer 的预算")
	}
	if take("g1") {
		t.Fatal("R2 不该动别的 peer 的预算")
	}

	if _, err := db.ClearAllData(ctx); err != nil {
		t.Fatalf("R3: %v", err)
	}
	if !take("g1") {
		t.Fatal("R3 应清空全部预算")
	}
}

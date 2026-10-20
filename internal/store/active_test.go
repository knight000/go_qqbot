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
		ok, err := db.TakeActiveBudget(ctx, "2026-10-20", "c2c", "u1", 2)
		if err != nil {
			t.Fatalf("第 %d 次占额度出错: %v", i, err)
		}
		if !ok {
			t.Fatalf("第 %d 次应占额度成功", i)
		}
	}
	ok, err := db.TakeActiveBudget(ctx, "2026-10-20", "c2c", "u1", 2)
	if err != nil {
		t.Fatalf("超额占额度出错: %v", err)
	}
	if ok {
		t.Fatal("limit=2 时第 3 次必须失败")
	}

	// 维度是 (day, scene, peer)：换一天、换场景、换 peer 都不受影响。
	for _, c := range []struct{ day, scene, peer string }{
		{"2026-10-21", "c2c", "u1"},
		{"2026-10-20", "group", "u1"},
		{"2026-10-20", "c2c", "u2"},
	} {
		got, err := db.TakeActiveBudget(ctx, c.day, c.scene, c.peer, 2)
		if err != nil || !got {
			t.Fatalf("独立维度 %v 应可占额度: ok=%v err=%v", c, got, err)
		}
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
	if ok, err := db.TakeActiveBudget(ctx, "2026-10-20", "group", "g1", 1); err != nil || !ok {
		t.Fatalf("首次应可占额度: ok=%v err=%v", ok, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关库: %v", err)
	}

	db2 := open()
	defer db2.Close()
	if ok, err := db2.TakeActiveBudget(ctx, "2026-10-20", "group", "g1", 1); err != nil || ok {
		t.Fatalf("重开后额度应已用尽: ok=%v err=%v", ok, err)
	}
}

// 预算属于业务数据：R2 清该 peer、R3 清全表；R1（会话状态）与 R0（重连）不动它。
func TestResetLevelsAndActiveBudget(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const day = "2026-10-20"

	used := func(peer string) int {
		t.Helper()
		n, err := db.ActiveUsed(ctx, day, "group", peer)
		if err != nil {
			t.Fatalf("读取 %s 的额度: %v", peer, err)
		}
		return n
	}
	for _, peer := range []string{"g1", "g2"} {
		if ok, err := db.TakeActiveBudget(ctx, day, "group", peer, 1); err != nil || !ok {
			t.Fatalf("占 %s 的额度失败: ok=%v err=%v", peer, ok, err)
		}
	}

	if _, err := db.ClearPeerSession(ctx, "g1"); err != nil {
		t.Fatalf("R1: %v", err)
	}
	if got := used("g1"); got != 1 {
		t.Fatalf("R1 不该清空主动消息预算，实际 used=%d", got)
	}

	if _, err := db.ClearPeerData(ctx, "g2"); err != nil {
		t.Fatalf("R2: %v", err)
	}
	if got := used("g2"); got != 0 {
		t.Fatalf("R2 应清掉该 peer 的预算，实际 used=%d", got)
	}
	if got := used("g1"); got != 1 {
		t.Fatalf("R2 不该动别的 peer 的预算，实际 used=%d", got)
	}

	if _, err := db.ClearAllData(ctx); err != nil {
		t.Fatalf("R3: %v", err)
	}
	if got := used("g1"); got != 0 {
		t.Fatalf("R3 应清空全部预算，实际 used=%d", got)
	}
}

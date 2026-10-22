package store

import (
	"context"
	"testing"
)

func insertOrphanMedia(t *testing.T, db *DB, path string) {
	t.Helper()
	if _, err := db.w.ExecContext(context.Background(),
		`INSERT INTO media(message_id, sha256, kind, path, bytes, created_at) VALUES(NULL, ?, 'image', ?, 10, ?)`,
		"sha-"+path, path, nowUnix()); err != nil {
		t.Fatal(err)
	}
}

func countMedia(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.r.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM media`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// media.message_id 可空，这类行没有会话归属——**不属于任何一个 peer**。
//
// 因此 R2 的谓词只能是 `message_id IN (该 peer 的消息)`：把
// `message_id IS NULL` 也算进去，任一 peer 的重置都会删掉全库孤儿行，
// 预览还会给每个 peer 重复数一遍（10 个群各说"媒体 1 个"，实际删了 1 行）。
// 孤儿行只由 R3 的全库扫描（AllMediaPaths + ClearAllData）回收。
func TestR2LeavesMediaWithoutMessageToR3(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	m1 := insertTestMessage(t, db, "e1", "g1")
	insertTestMedia(t, db, m1, "/tmp/a.jpg")
	insertOrphanMedia(t, db, "/tmp/orphan.jpg")

	paths, err := db.PeerMediaPaths(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(paths, "/tmp/a.jpg") {
		t.Fatalf("R2 应包含该 peer 自己的媒体，实际 %v", paths)
	}
	if contains(paths, "/tmp/orphan.jpg") {
		t.Fatalf("R2 不应包含无归属媒体（它不是 g1 的数据），实际 %v", paths)
	}

	stats, err := db.PeerStats(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.MediaRows != 1 {
		t.Fatalf("g1 的预览媒体数 = %d, want 1", stats.MediaRows)
	}
	// 孤儿行没有归属，任何 peer 都不该看到它——否则 N 个 peer 的预览会
	// 把同一行数 N 遍。
	other, err := db.PeerStats(ctx, "g2")
	if err != nil {
		t.Fatal(err)
	}
	if other.MediaRows != 0 {
		t.Fatalf("无关 peer 的预览媒体数 = %d, want 0（孤儿行不得被重复计数）", other.MediaRows)
	}

	counts, err := db.ClearPeerData(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if counts.MediaRows != 1 {
		t.Fatalf("清理媒体行数 = %d, want 1（与实际删除一致）", counts.MediaRows)
	}
	if n := countMedia(t, db); n != 1 {
		t.Fatalf("R2 后应剩 1 行无归属媒体，实际 %d 行", n)
	}

	// 孤儿行的回收路径是 R3：全库扫描拿到路径、全库删拿到行。
	all, err := db.AllMediaPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(all, "/tmp/orphan.jpg") {
		t.Fatalf("R3 的全库扫描应能拿到无归属媒体，实际 %v", all)
	}
	allCounts, err := db.ClearAllData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if allCounts.MediaRows != 1 {
		t.Fatalf("R3 应清掉那 1 行无归属媒体，实际 %d", allCounts.MediaRows)
	}
	if n := countMedia(t, db); n != 0 {
		t.Fatalf("R3 后不应再有媒体行，剩余 %d", n)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

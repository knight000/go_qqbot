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

// media.message_id 可空：一旦有行没挂到消息上，INNER JOIN 的预览与删除都会
// 漏掉它——预览说"媒体 0 个"，重置完行和文件都还在。这类行没有会话归属，
// 任何一次 R2 都命中不了它们，只能在 R2 时一并回收。
func TestR2CoversMediaWithoutMessage(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	m1 := insertTestMessage(t, db, "e1", "g1")
	insertTestMedia(t, db, m1, "/tmp/a.jpg")
	insertOrphanMedia(t, db, "/tmp/orphan.jpg")

	paths, err := db.PeerMediaPaths(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(paths, "/tmp/orphan.jpg") || !contains(paths, "/tmp/a.jpg") {
		t.Fatalf("预览应包含无归属媒体，实际 %v", paths)
	}

	stats, err := db.PeerStats(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.MediaRows != 2 {
		t.Fatalf("预览媒体数 = %d, want 2", stats.MediaRows)
	}

	counts, err := db.ClearPeerData(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if counts.MediaRows != 2 {
		t.Fatalf("清理媒体行数 = %d, want 2", counts.MediaRows)
	}
	if n := countMedia(t, db); n != 0 {
		t.Fatalf("R2 应清掉无归属媒体，剩余 %d 行", n)
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

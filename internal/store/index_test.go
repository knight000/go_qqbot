package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// explain 返回 EXPLAIN QUERY PLAN 的 detail 列。用真实执行计划断言索引被用上：
// 只检查 sqlite_master 里有没有索引，无法发现"索引建了但查询走全表扫"。
func explain(t *testing.T, db *DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.r.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("读取执行计划: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var (
			id, parent, unused int
			detail             string
		)
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("解析执行计划: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历执行计划: %v", err)
	}
	return strings.Join(details, " | ")
}

func indexSQL(t *testing.T, db *DB, name string) (string, bool) {
	t.Helper()
	var sql string
	err := db.r.QueryRowContext(context.Background(),
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&sql)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", false
		}
		t.Fatalf("读取索引 %s: %v", name, err)
	}
	return sql, true
}

// job_runs 是唯一按"每次执行"增长的表，GC 按 started_at 删。原来的
// (job_id, started_at) 索引没有任何查询用得上，删过期行只能全表扫。
func TestJobRunsGCPurgeUsesStartedAtIndex(t *testing.T) {
	db := openTestDB(t)

	plan := explain(t, db, purgeJobRunsSQL, 0, 100)
	if !strings.Contains(plan, "idx_job_runs_started") {
		t.Errorf("按 started_at 清理应走 idx_job_runs_started，实际计划: %s", plan)
	}
	if sql, ok := indexSQL(t, db, "idx_job_runs"); ok {
		t.Errorf("旧的 idx_job_runs 应被删除，实际仍在: %s", sql)
	}
}

// 旧库带着 idx_job_runs 升级：索引必须被换掉，且换完后新查询确实用新索引。
func TestMigrateReplacesLegacyJobRunsIndex(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.w.ExecContext(ctx, `CREATE TABLE job_runs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id      TEXT NOT NULL,
		started_at  INTEGER NOT NULL,
		finished_at INTEGER,
		status      TEXT,
		error       TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.ExecContext(ctx,
		`CREATE INDEX idx_job_runs ON job_runs(job_id, started_at)`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("重复迁移: %v", err)
	}

	if _, ok := indexSQL(t, db, "idx_job_runs"); ok {
		t.Error("旧索引应在迁移后消失")
	}
	if _, ok := indexSQL(t, db, "idx_job_runs_started"); !ok {
		t.Fatal("迁移后应存在 idx_job_runs_started")
	}
	plan := explain(t, db, purgeJobRunsSQL, 0, 100)
	if !strings.Contains(plan, "idx_job_runs_started") {
		t.Errorf("旧库升级后清理仍应走新索引，实际计划: %s", plan)
	}
}

package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUpsertJobKeepsPerPeerRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := nowUnix()

	for _, peer := range []string{"g1", "g2", ""} {
		if err := db.UpsertJob(ctx, Job{
			ID: "digest:daily", Peer: peer, Plugin: "digest",
			Spec: "0 9 * * *", NextRunAt: now + 60,
		}); err != nil {
			t.Fatalf("写入 %q: %v", peer, err)
		}
	}
	// 同一任务在不同会话上是不同的行，重新注册一个不能顶掉另一个。
	if err := db.UpsertJob(ctx, Job{
		ID: "digest:daily", Peer: "g1", Plugin: "digest",
		Spec: "0 10 * * *", NextRunAt: now + 120, Payload: "changed",
	}); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE id = 'digest:daily'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("应有 3 行实例，实际 %d", n)
	}
	var spec string
	if err := db.r.QueryRowContext(ctx,
		`SELECT spec FROM jobs WHERE id = 'digest:daily' AND peer = 'g2'`).Scan(&spec); err != nil {
		t.Fatal(err)
	}
	if spec != "0 9 * * *" {
		t.Fatalf("g2 的定义被其他 peer 的注册覆盖了: %q", spec)
	}
}

func TestDueJobsSkipsLeftGroups(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := nowUnix()

	for _, peer := range []string{"g1", "g2"} {
		if err := db.MarkGroupPresent(ctx, peer); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertJob(ctx, Job{
			ID: "digest:daily", Peer: peer, Plugin: "digest",
			Spec: "0 9 * * *", NextRunAt: now - 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertJob(ctx, Job{
		ID: "digest:global", Plugin: "digest", Spec: "0 9 * * *", NextRunAt: now - 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGroupLeft(ctx, "g2"); err != nil {
		t.Fatal(err)
	}

	jobs, err := db.DueJobs(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, j := range jobs {
		got[j.ID+"@"+j.Peer] = true
	}
	if !got["digest:daily@g1"] {
		t.Fatal("在群的任务应该到期")
	}
	if got["digest:daily@g2"] {
		t.Fatal("已退出的群不该再触发任务")
	}
	if !got["digest:global@"] {
		t.Fatal("全局任务不受群状态影响")
	}

	// 重新入群即恢复：不写任何 jobs 行，完全由 qq_groups 决定。
	if err := db.MarkGroupPresent(ctx, "g2"); err != nil {
		t.Fatal(err)
	}
	jobs, err = db.DueJobs(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]bool{}
	for _, j := range jobs {
		got[j.ID+"@"+j.Peer] = true
	}
	if !got["digest:daily@g2"] {
		t.Fatal("重新入群后任务应恢复")
	}
}

// UpsertJob 会把 next_run_at 重算成"下一个 cron 时刻"，运行期补实例时
// 不能用它：群消息每来一条就把任务推后一次，表现为永远不触发。
func TestInsertJobIfMissingDoesNotTouchExisting(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpsertJob(ctx, Job{
		ID: "digest:daily", Peer: "g1", Plugin: "digest",
		Spec: "0 9 * * *", NextRunAt: 100,
	}); err != nil {
		t.Fatal(err)
	}
	// 模拟熔断停用：这条状态不能被补建复活。
	if _, err := db.w.ExecContext(ctx, `UPDATE jobs SET enabled = 0 WHERE peer = 'g1'`); err != nil {
		t.Fatal(err)
	}

	inserted, err := db.InsertJobIfMissing(ctx, Job{
		ID: "digest:daily", Peer: "g1", Plugin: "digest",
		Spec: "0 9 * * *", NextRunAt: 999999,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("已存在的行不应再插入")
	}

	var (
		next    int64
		enabled int
	)
	if err := db.r.QueryRowContext(ctx,
		`SELECT next_run_at, enabled FROM jobs WHERE id = 'digest:daily' AND peer = 'g1'`).
		Scan(&next, &enabled); err != nil {
		t.Fatal(err)
	}
	if next != 100 || enabled != 0 {
		t.Fatalf("补建改动了已有行: next=%d enabled=%d", next, enabled)
	}

	// 缺失的行才真的插。
	inserted, err = db.InsertJobIfMissing(ctx, Job{
		ID: "digest:daily", Peer: "g2", Plugin: "digest",
		Spec: "0 9 * * *", NextRunAt: 200,
	})
	if err != nil || !inserted {
		t.Fatalf("缺失的实例应被补上: inserted=%v err=%v", inserted, err)
	}
}

func TestUpdateJobIsScopedToPeer(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, peer := range []string{"g1", "g2"} {
		if err := db.UpsertJob(ctx, Job{
			ID: "digest:daily", Peer: peer, Plugin: "digest",
			Spec: "0 9 * * *", NextRunAt: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpdateJob(ctx, Job{
		ID: "digest:daily", Peer: "g1", Enabled: false,
		NextRunAt: 200, LastRunAt: 150, LastStatus: "ok", ConsecutiveFailures: 2,
	}); err != nil {
		t.Fatal(err)
	}

	var (
		next     int64
		failures int
		enabled  int
	)
	if err := db.r.QueryRowContext(ctx,
		`SELECT next_run_at, consecutive_failures, enabled FROM jobs WHERE peer = 'g2'`).
		Scan(&next, &failures, &enabled); err != nil {
		t.Fatal(err)
	}
	if next != 100 || failures != 0 || enabled != 1 {
		t.Fatalf("更新 g1 影响了 g2: next=%d failures=%d enabled=%d", next, failures, enabled)
	}
}

func TestDisableJobsExceptKeepsPeerInstances(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, peer := range []string{"g1", "g2"} {
		if err := db.UpsertJob(ctx, Job{
			ID: "digest:daily", Peer: peer, Plugin: "digest",
			Spec: "0 9 * * *", NextRunAt: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertJob(ctx, Job{
		ID: "gone:tick", Peer: "g1", Plugin: "gone", Spec: "* * * * *", NextRunAt: 100,
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.DisableJobsExcept(ctx, []string{"digest:daily"}); err != nil {
		t.Fatal(err)
	}

	var enabled int
	if err := db.r.QueryRowContext(ctx,
		`SELECT enabled FROM jobs WHERE id = 'digest:daily' AND peer = 'g2'`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 {
		t.Fatal("仍在注册的插件，其 per-peer 实例不该被停用")
	}
	if err := db.r.QueryRowContext(ctx,
		`SELECT enabled FROM jobs WHERE id = 'gone:tick'`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Fatal("插件已被删除的任务应停用")
	}
}

func TestClearPeerDataRemovesJobRuns(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, peer := range []string{"g1", "g2"} {
		if err := db.RecordJobRun(ctx, "digest:daily", peer, 1, 2, "ok", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordJobRun(ctx, "digest:global", "", 1, 2, "ok", ""); err != nil {
		t.Fatal(err)
	}

	counts, err := db.PeerStats(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if counts.JobRuns != 1 {
		t.Fatalf("预览应统计到 1 条任务记录，实际 %d", counts.JobRuns)
	}

	counts, err = db.ClearPeerData(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if counts.JobRuns != 1 {
		t.Fatalf("应清掉 1 条任务记录，实际 %d", counts.JobRuns)
	}

	var n int
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_runs WHERE peer = 'g1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("g1 的任务记录没清干净")
	}
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_runs WHERE peer = 'g2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("R2 不该动其他群的记录")
	}
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_runs WHERE peer = ''`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("R2 不该动全局任务记录")
	}
}

// 旧库的 jobs 是 id 单主键、没有 peer：升级后行必须还在，且能按 peer 再插一条。
func TestMigrateLegacyJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.w.ExecContext(ctx, `CREATE TABLE jobs (
		id                   TEXT PRIMARY KEY,
		plugin               TEXT NOT NULL,
		spec                 TEXT NOT NULL,
		timezone             TEXT NOT NULL DEFAULT 'Asia/Shanghai',
		enabled              INTEGER NOT NULL DEFAULT 1,
		next_run_at          INTEGER NOT NULL,
		last_run_at          INTEGER,
		last_status          TEXT,
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		payload              TEXT
	)`); err != nil {
		t.Fatal(err)
	}
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
		`INSERT INTO jobs(id, plugin, spec, next_run_at, last_status) VALUES('old:tick', 'old', '* * * * *', 42, 'ok')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO job_runs(job_id, started_at, status) VALUES('old:tick', 41, 'ok')`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	// 迁移是幂等的：重复执行不能因为表已重建而报错。
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("重复迁移: %v", err)
	}

	var (
		peer     string
		next     int64
		status   string
		runPeers int
	)
	if err := db.r.QueryRowContext(ctx,
		`SELECT peer, next_run_at, last_status FROM jobs WHERE id = 'old:tick'`).
		Scan(&peer, &next, &status); err != nil {
		t.Fatal(err)
	}
	if peer != "" || next != 42 || status != "ok" {
		t.Fatalf("旧任务搬运有损: peer=%q next=%d status=%q", peer, next, status)
	}
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_runs WHERE job_id = 'old:tick' AND peer = ''`).Scan(&runPeers); err != nil {
		t.Fatal(err)
	}
	if runPeers != 1 {
		t.Fatalf("旧执行历史应搬进 peer='': %d", runPeers)
	}

	// 新主键真的生效了：同 id 不同 peer 是两行。
	if err := db.UpsertJob(ctx, Job{
		ID: "old:tick", Peer: "g1", Plugin: "old", Spec: "* * * * *", NextRunAt: 43,
	}); err != nil {
		t.Fatalf("复合主键未生效: %v", err)
	}

	// 重建表会带走索引，必须补回来，否则扫描退化成全表扫。
	var idx int
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_jobs_next'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatal("迁移后丢了 idx_jobs_next")
	}
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_job_runs_peer'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatal("迁移后缺少 idx_job_runs_peer")
	}
}

// 正在跑的插件必须能在 SQL 层被排除，而不是取回来再在内存里丢掉。
//
// 只在内存里丢会饿死别人：limit 是按 next_run_at 排的，一个积压了上百行
// per-peer 实例的插件会把整个窗口占满，其他插件的到期任务永远轮不到。
func TestDueJobsExcludesBusyPlugins(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := nowUnix()

	for _, peer := range []string{"a1", "a2", "a3"} {
		if err := db.MarkGroupPresent(ctx, peer); err != nil {
			t.Fatal(err)
		}
	}
	// 三个 digest 行都比 weather 行更早到期：limit=2 时窗口会被它独占。
	for _, peer := range []string{"a1", "a2"} {
		if err := db.UpsertJob(ctx, Job{
			ID: "digest:daily", Peer: peer, Plugin: "digest",
			Spec: "0 9 * * *", NextRunAt: now - 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertJob(ctx, Job{
		ID: "weather:hourly", Peer: "a3", Plugin: "weather",
		Spec: "0 * * * *", NextRunAt: now - 10,
	}); err != nil {
		t.Fatal(err)
	}

	jobs, err := db.DueJobs(ctx, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("limit=2 应取两行，实际 %d", len(jobs))
	}

	jobs, err = db.DueJobs(ctx, now, 2, "digest")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Plugin != "weather" {
		t.Fatalf("排除了 digest 之后应只剩 weather: %+v", jobs)
	}

	jobs, err = db.DueJobs(ctx, now, 2, "digest", "weather")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("两个插件都忙时应取不到任务: %+v", jobs)
	}
}

package store

import (
	"context"
	"fmt"
	"strings"
)

// Job 对应 jobs 表。next_run_at 一律存 Unix 秒，时区显式存字段。
//
// Peer 是任务的会话维度（ARCHITECTURE.md §5.1）：
//   - 空串 = 全局任务，只执行一次，插件自己决定推给谁；
//   - 非空 = 该会话的实例，执行时 Host 把 JobSpec.Peer 填成它。
type Job struct {
	ID                  string
	Peer                string
	Plugin              string
	Spec                string
	Timezone            string
	Enabled             bool
	NextRunAt           int64
	LastRunAt           int64
	LastStatus          string
	ConsecutiveFailures int
	Payload             string
}

// UpsertJob 注册任务定义：新任务默认启用；已存在的任务只更新定义，
// 保留 enabled / 失败计数，避免一次重启悄悄绕过熔断。
func (db *DB) UpsertJob(ctx context.Context, j Job) error {
	if j.Timezone == "" {
		j.Timezone = "Asia/Shanghai"
	}
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO jobs(id, peer, plugin, spec, timezone, enabled, next_run_at, payload)
		 VALUES(?, ?, ?, ?, ?, 1, ?, ?)
		 ON CONFLICT(id, peer) DO UPDATE SET
			plugin      = excluded.plugin,
			spec        = excluded.spec,
			timezone    = excluded.timezone,
			next_run_at = excluded.next_run_at,
			payload     = excluded.payload`,
		j.ID, j.Peer, j.Plugin, j.Spec, j.Timezone, j.NextRunAt, j.Payload)
	if err != nil {
		return fmt.Errorf("注册任务 %s@%s: %w", j.ID, j.Peer, err)
	}
	return nil
}

// InsertJobIfMissing 只在行不存在时插入，返回是否真的插了。
//
// 与 UpsertJob 的区别：它绝不修改已存在的行。运行期给新入群的群补 per-peer
// 实例时必须用它——UpsertJob 会把 next_run_at 重算成"下一个 cron 时刻"，
// 群消息一密集就会把任务一直往后推，表现为"这个群永远收不到定时推送"。
func (db *DB) InsertJobIfMissing(ctx context.Context, j Job) (bool, error) {
	if j.Timezone == "" {
		j.Timezone = "Asia/Shanghai"
	}
	res, err := db.w.ExecContext(ctx,
		`INSERT INTO jobs(id, peer, plugin, spec, timezone, enabled, next_run_at, payload)
		 VALUES(?, ?, ?, ?, ?, 1, ?, ?)
		 ON CONFLICT(id, peer) DO NOTHING`,
		j.ID, j.Peer, j.Plugin, j.Spec, j.Timezone, j.NextRunAt, j.Payload)
	if err != nil {
		return false, fmt.Errorf("补任务实例 %s@%s: %w", j.ID, j.Peer, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DueJobs 取到期任务。
//
// per-peer 行要求对应群仍在群（left_at = 0）：退群即停、重新入群即恢复，
// 不需要另一份"该群任务已停用"的状态。熔断（enabled=0）与未注册是别的维度，
// 两者互不覆盖——如果退群也写 enabled=0，入群恢复时就分不清该不该复活。
func (db *DB) DueJobs(ctx context.Context, now int64, limit int) ([]Job, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT j.id, j.peer, j.plugin, j.spec, j.timezone, j.enabled, j.next_run_at,
		        COALESCE(j.last_run_at, 0), COALESCE(j.last_status, ''), j.consecutive_failures, COALESCE(j.payload, '')
		 FROM jobs j
		 WHERE j.enabled = 1 AND j.next_run_at <= ?
		   AND (j.peer = '' OR EXISTS (
		         SELECT 1 FROM qq_groups g
		          WHERE g.group_openid = j.peer AND g.left_at = 0))
		 ORDER BY j.next_run_at LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("扫描到期任务: %w", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		var j Job
		var enabled int
		if err := rows.Scan(&j.ID, &j.Peer, &j.Plugin, &j.Spec, &j.Timezone, &enabled, &j.NextRunAt,
			&j.LastRunAt, &j.LastStatus, &j.ConsecutiveFailures, &j.Payload); err != nil {
			return nil, fmt.Errorf("读取任务: %w", err)
		}
		j.Enabled = enabled == 1
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateJob 按 (id, peer) 更新运行状态。只按 id 更新会一次改掉所有群的行。
func (db *DB) UpdateJob(ctx context.Context, j Job) error {
	_, err := db.w.ExecContext(ctx,
		`UPDATE jobs SET enabled = ?, next_run_at = ?, last_run_at = ?, last_status = ?, consecutive_failures = ?
		 WHERE id = ? AND peer = ?`,
		boolInt(j.Enabled), j.NextRunAt, j.LastRunAt, j.LastStatus, j.ConsecutiveFailures, j.ID, j.Peer)
	if err != nil {
		return fmt.Errorf("更新任务 %s@%s: %w", j.ID, j.Peer, err)
	}
	return nil
}

func (db *DB) RecordJobRun(ctx context.Context, jobID, peer string, started, finished int64, status, errMsg string) error {
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO job_runs(job_id, peer, started_at, finished_at, status, error) VALUES(?, ?, ?, ?, ?, ?)`,
		jobID, peer, started, finished, status, errMsg)
	if err != nil {
		return fmt.Errorf("记录任务执行 %s@%s: %w", jobID, peer, err)
	}
	return nil
}

// CountJobRuns 统计某会话的执行历史，供 R2 预览与执行前后对账。
func (db *DB) CountJobRuns(ctx context.Context, peer string) (int64, error) {
	var n int64
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_runs WHERE peer = ?`, peer).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计任务记录: %w", err)
	}
	return n, nil
}

// DisableJobsExcept 停用不再由任何插件注册的任务（插件被删除后的清理）。
//
// keep 里是不带 peer 的任务 id（<plugin>:<name>），per-peer 实例行的 id 相同，
// 因此会一并保留——停用它们等于"插件还在，但它的所有群实例都停了"，
// 与"插件被删除"是两件事。
func (db *DB) DisableJobsExcept(ctx context.Context, keep []string) error {
	query := `UPDATE jobs SET enabled = 0`
	var args []any
	if len(keep) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
		query += ` WHERE id NOT IN (` + placeholders + `)`
		for _, id := range keep {
			args = append(args, id)
		}
	}
	if _, err := db.w.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("停用未注册任务: %w", err)
	}
	return nil
}

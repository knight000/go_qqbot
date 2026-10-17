package store

import (
	"context"
	"fmt"
	"strings"
)

// Job 对应 jobs 表。next_run_at 一律存 Unix 秒，时区显式存字段。
type Job struct {
	ID                  string
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

// UpsertJob 注册任务：新任务默认启用；已存在的任务只更新定义，
// 保留 enabled / 失败计数，避免一次重启悄悄绕过熔断。
func (db *DB) UpsertJob(ctx context.Context, j Job) error {
	if j.Timezone == "" {
		j.Timezone = "Asia/Shanghai"
	}
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO jobs(id, plugin, spec, timezone, enabled, next_run_at, payload)
		 VALUES(?, ?, ?, ?, 1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			plugin      = excluded.plugin,
			spec        = excluded.spec,
			timezone    = excluded.timezone,
			next_run_at = excluded.next_run_at,
			payload     = excluded.payload`,
		j.ID, j.Plugin, j.Spec, j.Timezone, j.NextRunAt, j.Payload)
	if err != nil {
		return fmt.Errorf("注册任务 %s: %w", j.ID, err)
	}
	return nil
}

func (db *DB) DueJobs(ctx context.Context, now int64, limit int) ([]Job, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT id, plugin, spec, timezone, enabled, next_run_at,
		        COALESCE(last_run_at, 0), COALESCE(last_status, ''), consecutive_failures, COALESCE(payload, '')
		 FROM jobs WHERE enabled = 1 AND next_run_at <= ?
		 ORDER BY next_run_at LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("扫描到期任务: %w", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		var j Job
		var enabled int
		if err := rows.Scan(&j.ID, &j.Plugin, &j.Spec, &j.Timezone, &enabled, &j.NextRunAt,
			&j.LastRunAt, &j.LastStatus, &j.ConsecutiveFailures, &j.Payload); err != nil {
			return nil, fmt.Errorf("读取任务: %w", err)
		}
		j.Enabled = enabled == 1
		out = append(out, j)
	}
	return out, rows.Err()
}

func (db *DB) UpdateJob(ctx context.Context, j Job) error {
	_, err := db.w.ExecContext(ctx,
		`UPDATE jobs SET enabled = ?, next_run_at = ?, last_run_at = ?, last_status = ?, consecutive_failures = ?
		 WHERE id = ?`,
		boolInt(j.Enabled), j.NextRunAt, j.LastRunAt, j.LastStatus, j.ConsecutiveFailures, j.ID)
	if err != nil {
		return fmt.Errorf("更新任务 %s: %w", j.ID, err)
	}
	return nil
}

func (db *DB) RecordJobRun(ctx context.Context, jobID string, started, finished int64, status, errMsg string) error {
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO job_runs(job_id, started_at, finished_at, status, error) VALUES(?, ?, ?, ?, ?)`,
		jobID, started, finished, status, errMsg)
	if err != nil {
		return fmt.Errorf("记录任务执行 %s: %w", jobID, err)
	}
	return nil
}

// DisableJobsExcept 停用不再由任何插件注册的任务（插件被删除后的清理）。
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

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 重置的四级语义与三条硬约定见 ARCHITECTURE.md §8.3。
//
// 本文件只负责"把数据删干净"和"把这次操作记下来"，不含任何权限、冷却、
// 确认码的判断——那些在 core。这样 store 的每个函数都可以被单独验证
// "说删多少就删多少"，不必先构造一套指令上下文。

// 重置级别。取值同时是审计表里的 level 字段，改动等于改历史记录语义。
const (
	ResetLevelReconnect = "reconnect" // R0：只重连，不动数据
	ResetLevelSession   = "session"   // R1：清该会话的插件 KV
	ResetLevelData      = "data"      // R2：清该会话的消息与媒体
	ResetLevelAll       = "all"       // R3：清全部业务数据
)

// 审计状态机：pending → done | cancelled | expired。
const (
	ResetStatusPending   = "pending"
	ResetStatusDone      = "done"
	ResetStatusCancelled = "cancelled"
	ResetStatusExpired   = "expired"
)

// ResetScopeGlobal 是全局重置（R0 / R3）在审计与冷却里的 peer 取值。
//
// 用 '*' 而不是空串：空串会被读成"没记录"，而这里要表达的是
// "这次操作的作用域不属于任何一个会话"。
const ResetScopeGlobal = "*"

// ResetCounts 是一次清理的账。字段为 0 不是错误，是"本来就没有"。
type ResetCounts struct {
	Messages   int64
	MediaRows  int64
	JobRuns    int64
	Identities int64
	Events     int64
	KVRows     int64
}

func (c *ResetCounts) add(o ResetCounts) {
	c.Messages += o.Messages
	c.MediaRows += o.MediaRows
	c.JobRuns += o.JobRuns
	c.Identities += o.Identities
	c.Events += o.Events
	c.KVRows += o.KVRows
}

// Total 表示这次到底动了多少行；0 时调用方可以回"没有可清理的数据"。
func (c ResetCounts) Total() int64 {
	return c.Messages + c.MediaRows + c.JobRuns + c.Identities + c.Events + c.KVRows
}

// --- 媒体文件 ---

// PeerMediaPaths 返回某会话名下所有媒体文件路径。
//
// 必须在删行**之前**调用：媒体行一旦删掉，文件就成了无引用的孤儿，
// 只能靠扫盘发现，而扫盘在低配机上很贵（§8.3 第三条硬约定）。
func (db *DB) PeerMediaPaths(ctx context.Context, peer string) ([]string, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT m.path FROM media m
		 JOIN messages msg ON msg.id = m.message_id
		 WHERE msg.peer_openid = ? AND m.path <> ''`, peer)
	if err != nil {
		return nil, fmt.Errorf("读取 peer 媒体路径: %w", err)
	}
	return scanPaths(rows)
}

// AllMediaPaths 返回全库媒体文件路径，供 R3 使用。
func (db *DB) AllMediaPaths(ctx context.Context) ([]string, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT path FROM media WHERE path <> ''`)
	if err != nil {
		return nil, fmt.Errorf("读取媒体路径: %w", err)
	}
	return scanPaths(rows)
}

func scanPaths(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("解析媒体路径: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历媒体路径: %w", err)
	}
	return out, nil
}

// --- 分级清理 ---

// ClearPeerSession 是 R1：只删该会话私有的插件 KV。
// 插件级全局键（peer=”）不在此列——它们不属于任何一个会话。
func (db *DB) ClearPeerSession(ctx context.Context, peer string) (int64, error) {
	if peer == "" {
		return 0, fmt.Errorf("清理会话状态需要 peer")
	}
	res, err := db.w.ExecContext(ctx, `DELETE FROM plugin_kv WHERE peer = ?`, peer)
	if err != nil {
		return 0, fmt.Errorf("清理会话状态: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClearPeerData 是 R2：删该会话的消息与媒体行。
// 文件由调用方在调用本函数之前删除（先文件、后行）。
func (db *DB) ClearPeerData(ctx context.Context, peer string) (ResetCounts, error) {
	if peer == "" {
		return ResetCounts{}, fmt.Errorf("清理会话数据需要 peer")
	}
	var c ResetCounts

	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return c, fmt.Errorf("开启清理事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 先删媒体行（此时 messages 还在，子查询能取到 id），再删消息。
	res, err := tx.ExecContext(ctx,
		`DELETE FROM media WHERE message_id IN (SELECT id FROM messages WHERE peer_openid = ?)`, peer)
	if err != nil {
		return c, fmt.Errorf("清理媒体行: %w", err)
	}
	c.MediaRows, _ = res.RowsAffected()

	res, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE peer_openid = ?`, peer)
	if err != nil {
		return c, fmt.Errorf("清理消息: %w", err)
	}
	c.Messages, _ = res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return c, fmt.Errorf("提交清理事务: %w", err)
	}
	return c, nil
}

// ClearAllData 是 R3：清全部业务数据。
//
// **不动**的东西（每一条都是踩过或想清楚才会踩的坑）：
//   - admins     删了就永久锁在门外，而且它是自举的；
//   - bot_state  凭证/session/seq 是运维资产，重置不该把机器人搞成连不上；
//   - qq_groups  群名单是访问控制配置，清掉等于对所有群静默拉黑；
//   - jobs       任务定义，插件启动时会重新注册，删了只剩"重启前任务消失"；
//   - reset_audit 审计本身就是这次操作的记录，删了就等于没发生。
func (db *DB) ClearAllData(ctx context.Context) (ResetCounts, error) {
	var c ResetCounts

	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return c, fmt.Errorf("开启清理事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, t := range []struct {
		table string
		into  *int64
	}{
		{"media", &c.MediaRows},
		{"messages", &c.Messages},
		{"processed_events", &c.Events},
		{"plugin_kv", &c.KVRows},
		{"job_runs", &c.JobRuns},
		{"identities", &c.Identities},
	} {
		res, err := tx.ExecContext(ctx, `DELETE FROM `+t.table)
		if err != nil {
			return c, fmt.Errorf("清空 %s: %w", t.table, err)
		}
		*t.into, _ = res.RowsAffected()
	}

	if err := tx.Commit(); err != nil {
		return c, fmt.Errorf("提交清理事务: %w", err)
	}
	return c, nil
}

// AuditResetDone 直接记一条"已执行"。R0 / R1 不需要确认码，
// 没有 pending 阶段，但同样必须留痕——审计的意义是"所有重置都能查到"。
func (db *DB) AuditResetDone(ctx context.Context, actor, peer, level, detail string) error {
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO reset_audit(actor, peer, level, status, code, detail, created_at, expires_at, finished_at)
		 VALUES(?, ?, ?, ?, '', ?, ?, 0, ?)`,
		actor, peer, level, ResetStatusDone, detail, nowUnix(), nowUnix()); err != nil {
		return fmt.Errorf("写入重置审计: %w", err)
	}
	return nil
}

// PeerStats 返回 R2 会清掉多少行，用于执行前的预览。
func (db *DB) PeerStats(ctx context.Context, peer string) (ResetCounts, error) {
	var c ResetCounts
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE peer_openid = ?`, peer).Scan(&c.Messages); err != nil {
		return c, fmt.Errorf("统计 peer 消息: %w", err)
	}
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM media WHERE message_id IN (SELECT id FROM messages WHERE peer_openid = ?)`,
		peer).Scan(&c.MediaRows); err != nil {
		return c, fmt.Errorf("统计 peer 媒体: %w", err)
	}
	return c, nil
}

// DataStats 返回 R3 会清掉多少行，用于执行前的预览。
func (db *DB) DataStats(ctx context.Context) (ResetCounts, error) {
	var c ResetCounts
	for _, t := range []struct {
		table string
		into  *int64
	}{
		{"messages", &c.Messages},
		{"media", &c.MediaRows},
		{"processed_events", &c.Events},
		{"plugin_kv", &c.KVRows},
		{"job_runs", &c.JobRuns},
		{"identities", &c.Identities},
	} {
		if err := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t.table).Scan(t.into); err != nil {
			return c, fmt.Errorf("统计 %s: %w", t.table, err)
		}
	}
	return c, nil
}

// --- 审计与确认码 ---

// ResetRecord 是 reset_audit 的一行。
type ResetRecord struct {
	ID         int64
	Actor      string
	Peer       string
	Level      string
	Status     string
	Code       string
	Detail     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	FinishedAt time.Time
}

// OpenResetPending 开一条待确认记录，并把它之前的同目标 pending 置为 cancelled。
//
// 顶掉旧记录而不是拒绝新请求：用户很可能就是没收到上一条消息，
// 拒绝只会让他在同一个死胡同里反复试。旧的那条仍然留痕。
func (db *DB) OpenResetPending(ctx context.Context, actor, peer, level, code string, ttl time.Duration) (int64, error) {
	now := time.Now()
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启审计事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE reset_audit SET status = ?, finished_at = ?
		 WHERE status = ? AND actor = ? AND peer = ? AND level = ?`,
		ResetStatusCancelled, now.Unix(), ResetStatusPending, actor, peer, level); err != nil {
		return 0, fmt.Errorf("作废旧确认码: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO reset_audit(actor, peer, level, status, code, detail, created_at, expires_at, finished_at)
		 VALUES(?, ?, ?, ?, ?, '', ?, ?, 0)`,
		actor, peer, level, ResetStatusPending, code, now.Unix(), now.Add(ttl).Unix())
	if err != nil {
		return 0, fmt.Errorf("写入重置审计: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("读取审计 id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交审计: %w", err)
	}
	return id, nil
}

// PendingReset 取最近一条未被消费的 pending 记录（按 actor + peer + level 绑定）。
// 调用方负责比对确认码与过期时间。
func (db *DB) PendingReset(ctx context.Context, actor, peer, level string) (ResetRecord, bool, error) {
	row := db.r.QueryRowContext(ctx,
		`SELECT id, actor, peer, level, status, code, COALESCE(detail, ''), created_at, expires_at, finished_at
		 FROM reset_audit
		 WHERE status = ? AND actor = ? AND peer = ? AND level = ?
		 ORDER BY id DESC LIMIT 1`,
		ResetStatusPending, actor, peer, level)
	r, err := scanReset(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ResetRecord{}, false, nil
		}
		return ResetRecord{}, false, fmt.Errorf("读取待确认重置: %w", err)
	}
	return r, true, nil
}

// CloseReset 结束一条审计记录（done / cancelled / expired）并补上执行摘要。
func (db *DB) CloseReset(ctx context.Context, id int64, status, detail string) error {
	if _, err := db.w.ExecContext(ctx,
		`UPDATE reset_audit SET status = ?, detail = ?, finished_at = ? WHERE id = ?`,
		status, detail, nowUnix(), id); err != nil {
		return fmt.Errorf("更新重置审计: %w", err)
	}
	return nil
}

// LastResetDoneAt 返回该 (level, peer) 最近一次成功执行的时间，用于冷却。
func (db *DB) LastResetDoneAt(ctx context.Context, level, peer string) (time.Time, bool, error) {
	var ts int64
	err := db.r.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(finished_at), 0) FROM reset_audit
		 WHERE level = ? AND peer = ? AND status = ?`,
		level, peer, ResetStatusDone).Scan(&ts)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("读取重置冷却: %w", err)
	}
	if ts == 0 {
		return time.Time{}, false, nil
	}
	return time.Unix(ts, 0), true, nil
}

// ExpirePendingResets 把超时未确认的记录标记为 expired，随 GC 跑。
// 不在读路径上做这件事，是为了让"尝试过但没确认"也留在审计里。
func (db *DB) ExpirePendingResets(ctx context.Context) (int64, error) {
	res, err := db.w.ExecContext(ctx,
		`UPDATE reset_audit SET status = ?, finished_at = ?
		 WHERE status = ? AND expires_at > 0 AND expires_at <= ?`,
		ResetStatusExpired, nowUnix(), ResetStatusPending, nowUnix())
	if err != nil {
		return 0, fmt.Errorf("过期待确认重置: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RecentResets 返回最近若干条审计，供 /reset status 展示。
func (db *DB) RecentResets(ctx context.Context, limit int) ([]ResetRecord, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := db.r.QueryContext(ctx,
		`SELECT id, actor, peer, level, status, code, COALESCE(detail, ''), created_at, expires_at, finished_at
		 FROM reset_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("读取重置审计: %w", err)
	}
	defer rows.Close()

	var out []ResetRecord
	for rows.Next() {
		r, err := scanReset(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanReset(scan func(...any) error) (ResetRecord, error) {
	var (
		r                     ResetRecord
		created, expires, fin int64
	)
	if err := scan(&r.ID, &r.Actor, &r.Peer, &r.Level, &r.Status, &r.Code, &r.Detail,
		&created, &expires, &fin); err != nil {
		return ResetRecord{}, err
	}
	r.CreatedAt = time.Unix(created, 0)
	if expires > 0 {
		r.ExpiresAt = time.Unix(expires, 0)
	}
	if fin > 0 {
		r.FinishedAt = time.Unix(fin, 0)
	}
	return r, nil
}

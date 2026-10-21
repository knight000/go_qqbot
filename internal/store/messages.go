package store

import (
	"context"
	"fmt"
	"time"
)

// Message 是落库的消息。content 会在写入前截断到 4 KB（§4.1）。
type Message struct {
	EventID      string
	MsgID        string
	Scene        string
	PeerOpenID   string
	SenderOpenID string
	MsgType      int
	Content      string
	HasMedia     bool
}

const maxContentBytes = 4 << 10

// InsertMessage 幂等写入：同一 event_id 重复到达不会报错。
func (db *DB) InsertMessage(ctx context.Context, m Message) error {
	content := truncateUTF8(m.Content, maxContentBytes)
	_, err := db.w.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages
			(event_id, msg_id, scene, peer_openid, sender_openid, msg_type, content, has_media, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.EventID, m.MsgID, m.Scene, m.PeerOpenID, m.SenderOpenID, m.MsgType, content, boolInt(m.HasMedia), nowUnix())
	if err != nil {
		return fmt.Errorf("写入消息: %w", err)
	}
	return nil
}

// MarkEventSeen 返回 true 表示首次见到该事件（需要处理）。
// resume 会重放事件，幂等表必须挡在插件之前。
func (db *DB) MarkEventSeen(ctx context.Context, eventID string) (bool, error) {
	res, err := db.w.ExecContext(ctx,
		`INSERT OR IGNORE INTO processed_events(event_id, received_at) VALUES(?, ?)`,
		eventID, nowUnix())
	if err != nil {
		return false, fmt.Errorf("事件去重: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("事件去重: %w", err)
	}
	return n == 1, nil
}

// PurgeMessages 分批删除过期消息。单事务大 DELETE 会长时间持写锁，
// 与同机 aria2 的 IO 叠加时容易出现 database is locked（§4.2）。
func (db *DB) PurgeMessages(ctx context.Context, before int64, limit int) (int64, error) {
	return db.purge(ctx,
		`DELETE FROM messages WHERE id IN (SELECT id FROM messages WHERE created_at < ? LIMIT ?)`,
		before, limit)
}

func (db *DB) PurgeEvents(ctx context.Context, before int64, limit int) (int64, error) {
	return db.purge(ctx,
		`DELETE FROM processed_events WHERE event_id IN
			(SELECT event_id FROM processed_events WHERE received_at < ? LIMIT ?)`,
		before, limit)
}

// purgeJobRunsSQL 是 job_runs 的分批清理语句。单独提成常量是为了让
// 「索引是否被这条查询用上」可以在测试里对着同一份 SQL 跑 EXPLAIN QUERY PLAN。
const purgeJobRunsSQL = `DELETE FROM job_runs WHERE id IN
			(SELECT id FROM job_runs WHERE started_at < ? LIMIT ?)`

func (db *DB) PurgeJobRuns(ctx context.Context, before int64, limit int) (int64, error) {
	return db.purge(ctx, purgeJobRunsSQL, before, limit)
}

// PurgeMediaRows 只删行；文件删除由媒体模块负责（本脚手架未启用下载）。
func (db *DB) PurgeMediaRows(ctx context.Context, before int64, limit int) (int64, error) {
	return db.purge(ctx,
		`DELETE FROM media WHERE id IN (SELECT id FROM media WHERE created_at < ? LIMIT ?)`,
		before, limit)
}

func (db *DB) purge(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := db.w.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("分批清理: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// DeletePeerMessages 用于 R2 重置（脚手架阶段尚未接指令，先提供能力）。
func (db *DB) DeletePeerMessages(ctx context.Context, peer string) (int64, error) {
	res, err := db.w.ExecContext(ctx, `DELETE FROM messages WHERE peer_openid = ?`, peer)
	if err != nil {
		return 0, fmt.Errorf("清理 peer 消息: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// IncrementalVacuum 回收空闲页，避免每月一次全库 VACUUM。
func (db *DB) IncrementalVacuum(ctx context.Context) error {
	if _, err := db.w.ExecContext(ctx, `PRAGMA incremental_vacuum(200)`); err != nil {
		return fmt.Errorf("incremental_vacuum: %w", err)
	}
	return nil
}

func nowUnix() int64 { return time.Now().Unix() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// 避免切断多字节字符
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

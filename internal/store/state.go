package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetState 读取 bot_state（token / session_id / seq / shard）。
func (db *DB) GetState(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := db.r.QueryRowContext(ctx, `SELECT v FROM bot_state WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取 bot_state[%s]: %w", key, err)
	}
	return v, true, nil
}

func (db *DB) PutState(ctx context.Context, key, value string) error {
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO bot_state(k, v, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(k) DO UPDATE SET v = excluded.v, updated_at = excluded.updated_at`,
		key, value, nowUnix())
	if err != nil {
		return fmt.Errorf("写入 bot_state[%s]: %w", key, err)
	}
	return nil
}

// ClaimStateValue 把 k 写成 v，并且**只在旧值不是 v 时**成功一次。
//
// 返回 true 表示"这一次是第一个把 v 写进去的调用者"，false 表示别人先写了。
// 用于"每天最多一次"这类动作（#67 的当日首次超限通知）：先 Get 再 Put 会让
// 两个并发协程都读到旧值、各发一条通知，而通知是发给人的——重复就是打扰。
//
// 并发安全靠 upsert 的 WHERE：SQLite 的写会串行化，第二个调用者的
// conflicting 行已经是 v，条件不成立，RowsAffected 为 0。
func (db *DB) ClaimStateValue(ctx context.Context, key, value string) (bool, error) {
	res, err := db.w.ExecContext(ctx,
		`INSERT INTO bot_state(k, v, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(k) DO UPDATE SET v = excluded.v, updated_at = excluded.updated_at
		 WHERE bot_state.v <> excluded.v`,
		key, value, nowUnix())
	if err != nil {
		return false, fmt.Errorf("认领 bot_state[%s]: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取 bot_state[%s] 认领结果: %w", key, err)
	}
	return n == 1, nil
}

func (db *DB) DeleteState(ctx context.Context, key string) error {
	if _, err := db.w.ExecContext(ctx, `DELETE FROM bot_state WHERE k = ?`, key); err != nil {
		return fmt.Errorf("删除 bot_state[%s]: %w", key, err)
	}
	return nil
}

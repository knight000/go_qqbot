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

func (db *DB) DeleteState(ctx context.Context, key string) error {
	if _, err := db.w.ExecContext(ctx, `DELETE FROM bot_state WHERE k = ?`, key); err != nil {
		return fmt.Errorf("删除 bot_state[%s]: %w", key, err)
	}
	return nil
}

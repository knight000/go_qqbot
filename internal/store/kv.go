package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KVGet 读取插件 KV；已过期视为不存在。
func (db *DB) KVGet(ctx context.Context, plugin, key string) ([]byte, bool, error) {
	var (
		val []byte
		exp sql.NullInt64
	)
	err := db.r.QueryRowContext(ctx,
		`SELECT v, expires_at FROM plugin_kv WHERE plugin = ? AND k = ?`, plugin, key).Scan(&val, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("读取 KV %s/%s: %w", plugin, key, err)
	}
	if exp.Valid && exp.Int64 > 0 && exp.Int64 <= time.Now().Unix() {
		return nil, false, nil
	}
	return val, true, nil
}

// KVSet 写入插件 KV，ttl<=0 表示永不过期。
func (db *DB) KVSet(ctx context.Context, plugin, key string, val []byte, ttl time.Duration) error {
	var exp any
	if ttl > 0 {
		exp = time.Now().Add(ttl).Unix()
	}
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO plugin_kv(plugin, k, v, expires_at, updated_at) VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(plugin, k) DO UPDATE SET v = excluded.v, expires_at = excluded.expires_at, updated_at = excluded.updated_at`,
		plugin, key, val, exp, nowUnix())
	if err != nil {
		return fmt.Errorf("写入 KV %s/%s: %w", plugin, key, err)
	}
	return nil
}

func (db *DB) KVDelete(ctx context.Context, plugin, key string) error {
	if _, err := db.w.ExecContext(ctx, `DELETE FROM plugin_kv WHERE plugin = ? AND k = ?`, plugin, key); err != nil {
		return fmt.Errorf("删除 KV %s/%s: %w", plugin, key, err)
	}
	return nil
}

// PurgeExpiredKV 清掉过期 KV，随 GC 一起跑。
func (db *DB) PurgeExpiredKV(ctx context.Context) (int64, error) {
	res, err := db.w.ExecContext(ctx,
		`DELETE FROM plugin_kv WHERE expires_at IS NOT NULL AND expires_at > 0 AND expires_at <= ?`,
		nowUnix())
	if err != nil {
		return 0, fmt.Errorf("清理过期 KV: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

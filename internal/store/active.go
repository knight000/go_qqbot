package store

import (
	"context"
	"fmt"
)

// TakeActiveBudget 原子地占一格主动消息日预算。
//
// 返回 false 表示当日额度已满（不是错误）；返回 error 表示账本本身不可用，
// 调用方必须**拒绝发送**（fail-closed）——额度不可信时宁可少发一条，
// 也不能把平台的每日 1000 条配额当成无限量烧掉。
//
// "检查 + 自增"必须在同一条语句里完成：先 SELECT 再 UPDATE 会让两个
// 进程/协程读到同一个 used，然后各自发出一次，实际用量越过上限。
// INSERT ... ON CONFLICT ... DO UPDATE ... WHERE used < ? 的 RowsAffected
// 正好是"这次有没有占到"，等价于一次条件自增。
//
// limit <= 0 表示不限量，此时仍然记账（便于事后核对），只是永不拒绝。
func (db *DB) TakeActiveBudget(ctx context.Context, day, scene, peer string, limit int) (bool, error) {
	if limit <= 0 {
		if _, err := db.w.ExecContext(ctx,
			`INSERT INTO active_usage(day, scene, peer, used) VALUES(?, ?, ?, 1)
			 ON CONFLICT(day, scene, peer) DO UPDATE SET used = used + 1`,
			day, scene, peer); err != nil {
			return false, fmt.Errorf("记录主动消息额度: %w", err)
		}
		return true, nil
	}
	res, err := db.w.ExecContext(ctx,
		`INSERT INTO active_usage(day, scene, peer, used) VALUES(?, ?, ?, 1)
		 ON CONFLICT(day, scene, peer) DO UPDATE SET used = used + 1 WHERE used < ?`,
		day, scene, peer, limit)
	if err != nil {
		return false, fmt.Errorf("占用主动消息额度: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取主动消息额度结果: %w", err)
	}
	return n == 1, nil
}

// ActiveUsed 读回某个桶的已用额度，给日志与排障用。
func (db *DB) ActiveUsed(ctx context.Context, day, scene, peer string) (int, error) {
	var used int
	err := db.r.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(used), 0) FROM active_usage WHERE day = ? AND scene = ? AND peer = ?`,
		day, scene, peer).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("读取主动消息额度: %w", err)
	}
	return used, nil
}

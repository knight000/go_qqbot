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
// limit 必须 > 0：不限量时调用方（core.Sender）根本不会调这里，
// 它在 DailyPerPeer <= 0 时直接放行、不碰账本。
//
// 真传了 <= 0 就**显式拒发**，不能只靠 WHERE 恒假：upsert 的第一条
// INSERT 分支不受 WHERE 约束，新 key 会被插进一行 used=1 并返回成功。
func (db *DB) TakeActiveBudget(ctx context.Context, day, scene, peer string, limit int) (bool, error) {
	if limit <= 0 {
		return false, nil
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

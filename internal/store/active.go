package store

import (
	"context"
	"fmt"
)

// ActiveUsageKey 是主动消息日预算的维度：一天、一个场景、一个收件人。
//
// 三个字段都是 string，按位置传参时把 peer 和 scene 写反了编译器也看不出来，
// 而后果是"另一个人的额度被扣掉"。绑成一个类型后调用点必须写字段名，
// 记账与 R2 的"按 peer 清"也就用的是同一套坐标。
type ActiveUsageKey struct {
	// Day 是调度时区（Asia/Shanghai，与 cron 同源）的日期（YYYY-MM-DD）：
	// 跨日换桶靠它，不靠墙钟，也不跟进程本地时区。
	Day   string
	Scene string
	Peer  string
}

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
func (db *DB) TakeActiveBudget(ctx context.Context, key ActiveUsageKey, limit int) (bool, error) {
	if limit <= 0 {
		return false, nil
	}
	res, err := db.w.ExecContext(ctx,
		`INSERT INTO active_usage(day, scene, peer, used) VALUES(?, ?, ?, 1)
		 ON CONFLICT(day, scene, peer) DO UPDATE SET used = used + 1 WHERE used < ?`,
		key.Day, key.Scene, key.Peer, limit)
	if err != nil {
		return false, fmt.Errorf("占用主动消息额度: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取主动消息额度结果: %w", err)
	}
	return n == 1, nil
}

// RefundActiveBudget 归还一格主动消息日预算。
//
// 只给"平台明确拒绝"用：HTTP 通了但 err_code 非 0，说明这条消息没有发出去，
// 那一格不该记在账上。网络超时/5xx 不归还——请求可能已经到达平台，
// 还回去等于允许实际发送超过上限。
//
// used > 0 是必须的：没有对应的占用时归还只是 no-op，账不能变成负数，
// 否则"归还"会被一次写坏的调用放大成额外额度。
func (db *DB) RefundActiveBudget(ctx context.Context, key ActiveUsageKey) error {
	_, err := db.w.ExecContext(ctx,
		`UPDATE active_usage SET used = used - 1
		 WHERE day = ? AND scene = ? AND peer = ? AND used > 0`,
		key.Day, key.Scene, key.Peer)
	if err != nil {
		return fmt.Errorf("归还主动消息额度: %w", err)
	}
	return nil
}

// ActiveRateKey 是主动消息**每分钟**额度的维度：一分钟、一个场景。
//
// 没有 peer：平台频控是 Bot 级的（群聊 60 qpm、单聊 10 qps 都是整个 Bot 的
// 合计），按会话建桶等于把平台的额度乘以会话数——那正好是超发。
type ActiveRateKey struct {
	// Minute 是调度时区的分钟键（YYYY-MM-DDTHH:MM）：与日额度同源，
	// 所以跨时区部署不会在"哪一分钟"上分叉。
	Minute string
	Scene  string
}

// TakeActiveRate 原子地占一格分钟额度，返回 false 表示这一分钟已满。
//
// 与 TakeActiveBudget 完全同构（同一把"检查 + 自增"的 upsert、同一个
// RowsAffected 判据），理由是两条额度必须共享同一套并发语义：
// 先 SELECT 再 UPDATE 会让 N 个协程读到同一个 used，各自发一条。
func (db *DB) TakeActiveRate(ctx context.Context, key ActiveRateKey, limit int) (bool, error) {
	if limit <= 0 {
		return false, nil
	}
	res, err := db.w.ExecContext(ctx,
		`INSERT INTO active_rate(minute, scene, used) VALUES(?, ?, 1)
		 ON CONFLICT(minute, scene) DO UPDATE SET used = used + 1 WHERE used < ?`,
		key.Minute, key.Scene, limit)
	if err != nil {
		return false, fmt.Errorf("占用主动消息分钟额度: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取主动消息分钟额度结果: %w", err)
	}
	return n == 1, nil
}

// RefundActiveRate 归还一格分钟额度，语义与 RefundActiveBudget 一致：
// 只给"平台明确拒绝"用，used > 0 兜住账本为负。
func (db *DB) RefundActiveRate(ctx context.Context, key ActiveRateKey) error {
	_, err := db.w.ExecContext(ctx,
		`UPDATE active_rate SET used = used - 1
		 WHERE minute = ? AND scene = ? AND used > 0`,
		key.Minute, key.Scene)
	if err != nil {
		return fmt.Errorf("归还主动消息分钟额度: %w", err)
	}
	return nil
}

// PurgeActiveRate 清掉早于 before 的分钟桶（同一格式的分钟键）。
//
// 分钟桶只有一分钟的生命周期，留着只会在库里越积越多。清掉是安全的：
// 过期桶的下一次写入会新建一行 used=1，与"保留后被跨分钟复用"等价。
func (db *DB) PurgeActiveRate(ctx context.Context, before string) (int64, error) {
	res, err := db.w.ExecContext(ctx, `DELETE FROM active_rate WHERE minute < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("清理主动消息分钟额度: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理结果: %w", err)
	}
	return n, nil
}

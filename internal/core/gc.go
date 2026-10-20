package core

import (
	"context"
	"time"
)

// gcLoop 每 10 分钟清理一次过期数据（§4.2）。
// 一律分批删：单条大事务会长时间持有写锁，与同机 aria2 的磁盘 IO 叠加
// 时会造成 database is locked。
const (
	gcInterval = 10 * time.Minute
	gcBatch    = 5000
	gcMaxLoops = 20
)

func (c *Core) gcLoop(ctx context.Context) {
	t := time.NewTicker(gcInterval)
	defer t.Stop()

	c.runGC(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.runGC(ctx)
		}
	}
}

func (c *Core) runGC(ctx context.Context) {
	now := time.Now()
	msgBefore := now.Add(-c.cfg.Retention.Messages.Duration).Unix()
	evtBefore := now.Add(-c.cfg.Retention.Events.Duration).Unix()
	runBefore := now.Add(-c.cfg.Retention.JobRuns.Duration).Unix()

	msgN := c.purgeLoop(ctx, "messages", msgBefore)
	evtN := c.purgeLoop(ctx, "processed_events", evtBefore)
	runN := c.purgeLoop(ctx, "job_runs", runBefore)
	mediaN := c.purgeLoop(ctx, "media", msgBefore)

	kvN, err := c.store.PurgeExpiredKV(ctx)
	if err != nil {
		c.log.Warn("清理过期 KV 失败", "err", err)
	}
	// 未确认的重置请求留在审计表里，但状态要如实变成 expired——
	// 否则 /reset status 会把一条 60 秒前的请求显示成"待确认"。
	expN, err := c.store.ExpirePendingResets(ctx)
	if err != nil {
		c.log.Warn("过期待确认重置失败", "err", err)
	}
	// 会话行的读路径会自己删过期行，这里只兜底"再也没有人来读过"的那些。
	// 失败只告警：清不掉过期会话不影响任何正确性，下个周期还会再试。
	sessN, err := c.store.PurgeExpiredSessions(ctx)
	if err != nil {
		c.log.Warn("清理过期会话失败", "err", err)
	}

	if msgN+evtN+runN+mediaN+kvN+sessN > 0 {
		if err := c.store.IncrementalVacuum(ctx); err != nil {
			c.log.Warn("incremental_vacuum 失败", "err", err)
		}
		c.log.Info("GC 完成",
			"messages", msgN, "events", evtN, "job_runs", runN, "media", mediaN, "kv", kvN, "sessions", sessN)
	}
	if expN > 0 {
		c.log.Debug("已把过期的待确认重置标记为 expired", "count", expN)
	}
}

func (c *Core) purgeLoop(ctx context.Context, table string, before int64) int64 {
	var total int64
	for i := 0; i < gcMaxLoops; i++ {
		if ctx.Err() != nil {
			break
		}
		var (
			n   int64
			err error
		)
		switch table {
		case "messages":
			n, err = c.store.PurgeMessages(ctx, before, gcBatch)
		case "processed_events":
			n, err = c.store.PurgeEvents(ctx, before, gcBatch)
		case "job_runs":
			n, err = c.store.PurgeJobRuns(ctx, before, gcBatch)
		case "media":
			n, err = c.store.PurgeMediaRows(ctx, before, gcBatch)
		}
		if err != nil {
			c.log.Warn("分批清理失败", "table", table, "err", err)
			break
		}
		total += n
		if n < gcBatch {
			break
		}
	}
	return total
}

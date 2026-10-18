package core

import "context"

// per-peer 任务实例的补建。
//
// 调用时机只有两个，且都由 store 收成一个布尔值（ObserveGroup 的 changed）：
//   - 首次见到某个群；
//   - 某个群从"已退出"恢复成"在群"（退群事件丢失时的自愈路径）。
//
// 普通消息不会走到这里，所以不需要在内存里再记一份"已展开的群"——
// 那份状态一旦与数据库不一致（比如重启前补建失败），反而会永久挡住补建。
//
// 退群不需要做任何事：DueJobs 会 join qq_groups 要求 left_at = 0，
// 行还在，只是不再到期。

// expandPeerJobs 为某个群补建所有 per-peer 任务的实例行。幂等。
func (c *Core) expandPeerJobs(ctx context.Context, peer string) {
	if peer == "" || c.sched == nil {
		return
	}
	if err := c.sched.ExpandPeer(ctx, peer); err != nil {
		// 补建失败不阻断消息处理：实例行只影响这个群的定时推送，
		// 下一条消息或下次重启（Register 会遍历所有在群）都会重试。
		c.log.Warn("补建任务群实例失败", "group", peer, "err", err)
	}
}

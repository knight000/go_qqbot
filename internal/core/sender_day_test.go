package core

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

// recordingUsage 只记下每次记账的 key，不落库：切日口径的断言要直接看到
// Day 字符串，而不是拐弯去查表。计数按 Day 分桶，语义与真账本一致。
type recordingUsage struct {
	keys   []store.ActiveUsageKey
	counts map[string]int
}

func (r *recordingUsage) TakeActiveBudget(_ context.Context, key store.ActiveUsageKey, limit int) (bool, error) {
	r.keys = append(r.keys, key)
	if r.counts == nil {
		r.counts = make(map[string]int)
	}
	if r.counts[key.Day] >= limit {
		return false, nil
	}
	r.counts[key.Day]++
	return true, nil
}

// 日预算的切日必须与 cron 用同一个时区来源。
//
// 部署单元在 UTC 机器上跑（Debian 12 默认 UTC），cron 却按 Asia/Shanghai
// 执行：预算若按进程本地时区切日，就在北京时间 08:00 换桶，而平台配额按
// 自然日算——同一个平台自然日内可以花掉"前一天袋里的余额 + 08:00 后的
// 新满额"，最多 2×limit。
//
// 这里刻意让时钟的时区是 UTC、注入的调度时区是 Asia/Shanghai：
// 两个日期在 16:00Z 这一刻分叉，只有用调度时区格式化才能跨过去。
func TestActiveUsageDayUsesSchedulerLocation(t *testing.T) {
	// 调度器是 location 的唯一事实源，Sender 只是它的消费者。
	loc := NewScheduler(nil, slog.New(slog.DiscardHandler)).Location()
	if loc == nil || loc.String() != "Asia/Shanghai" {
		t.Fatalf("调度时区应为 Asia/Shanghai（与 cron 同源），实际 %v", loc)
	}

	usage := &recordingUsage{}
	s := NewSender(&fakeAPI{}, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 1,
	}, usage, loc)

	now := time.Date(2026, 10, 21, 15, 59, 0, 0, time.UTC) // 北京 10-21 23:59
	s.now = func() time.Time { return now }
	p := s.ForPlugin("digest")
	ctx := context.Background()

	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("首次主动消息不应失败: %v", err)
	}

	// UTC 还是 10-21，北京已经跨到 10-22：预算桶该跟着北京换。
	now = time.Date(2026, 10, 21, 16, 1, 0, 0, time.UTC) // 北京 10-22 00:01
	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("北京时间跨日后应重新有一份预算（UTC 同日不能算同一天）: %v", err)
	}

	want := []string{"2026-10-21", "2026-10-22"}
	if len(usage.keys) != len(want) {
		t.Fatalf("应记账 %d 次，实际 %d 次: %+v", len(want), len(usage.keys), usage.keys)
	}
	for i, day := range want {
		if got := usage.keys[i].Day; got != day {
			t.Fatalf("第 %d 次记账的 Day 应为北京时间 %s，实际 %s", i+1, day, got)
		}
	}
}

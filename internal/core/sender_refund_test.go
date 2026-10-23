package core

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// crossingUsage 在占格完成的那一刻把时钟推到次日——模拟"这条主动消息
// 从 23:59 发到 00:01"。归还若重算 key，就会落到一个 used==0 的次日桶。
type crossingUsage struct {
	recordingUsage
	afterTake func()
}

func (c *crossingUsage) TakeActiveBudget(ctx context.Context, key store.ActiveUsageKey, limit int) (bool, error) {
	ok, err := c.recordingUsage.TakeActiveBudget(ctx, key, limit)
	if c.afterTake != nil {
		c.afterTake()
	}
	return ok, err
}

// TestRefundCrossesMidnightIntoTakeBucket 钉住 #23 的时区版本缺陷：占格与
// 归还必须共用同一个 ActiveUsageKey。归还时重算 day 会把额度还进次日桶，
// 被 RefundActiveBudget 的 used>0 守卫兜成 no-op，旧桶则永远记着一笔
// 不存在的消耗——账面上当天少发一条，实际平台额度并没有被用掉。
func TestRefundCrossesMidnightIntoTakeBucket(t *testing.T) {
	loc := NewScheduler(nil, slog.New(slog.DiscardHandler)).Location()
	usage := &crossingUsage{}
	// 北京 10-21 23:59 占格；Take 返回后时钟走到 10-22 00:01。
	now := time.Date(2026, 10, 21, 15, 59, 0, 0, time.UTC)
	usage.afterTake = func() { now = time.Date(2026, 10, 21, 16, 1, 0, 0, time.UTC) }

	s := NewSender(&fakeAPI{bizSendAt: 1}, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 1,
	}, usage, loc)
	s.now = func() time.Time { return now }

	if err := s.ForPlugin("digest").SendActive(context.Background(), host.SceneC2C, "u1", host.Reply{Content: "x"}); err == nil {
		t.Fatal("平台业务错误应返回错误")
	}
	if len(usage.keys) != 1 {
		t.Fatalf("应只在占格时记账 1 次，实际 %d 次: %+v", len(usage.keys), usage.keys)
	}
	if got := usage.keys[0].Day; got != "2026-10-21" {
		t.Fatalf("占格应记在北京时间 10-21，实际 %s", got)
	}
	if n := usage.counts["2026-10-21"]; n != 0 {
		t.Fatalf("归还必须落回占格的那个桶（10-21 应为 0），实际 %d", n)
	}
	if n := usage.counts["2026-10-22"]; n != 0 {
		t.Fatalf("次日桶不该被碰到，实际 %d", n)
	}
}

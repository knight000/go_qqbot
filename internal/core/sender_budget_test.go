package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// 本文件是软预算（#67）的验收：超限拒发、被动不受影响、429 优先、
// 当天首次通知一次性、并发不超发。
//
// 所有额度账本用**真库**：这一票的正确性命题就是"库里那一次原子 upsert"，
// 用内存 fake 写出来的用例证明不了它在并发下成立。

func newBudgetSender(t *testing.T, f *fakeAPI, policy ActivePolicy, db *store.DB) *Sender {
	t.Helper()
	s := NewSender(f, slog.New(slog.DiscardHandler), policy, db, time.UTC)
	s.now = func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }
	return s
}

// 被动回复不占主动预算，也不该被它挡住：卡它等于无故降级用户体验。
func TestPassiveReplyIgnoresSoftBudget(t *testing.T) {
	f := &fakeAPI{}
	db := newUsageStore(t)
	s := newBudgetSender(t, f, ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 1,
		GroupPerMin:  1,
		SinglePerMin: 1,
	}, db)

	ev := &host.Event{MsgID: "msg-1", Scene: host.SceneGroup, PeerID: "g1"}
	for i := 0; i < 3; i++ {
		if err := s.Reply(context.Background(), ev, host.Reply{Content: "hi"}); err != nil {
			t.Fatalf("第 %d 条被动回复不该受主动预算影响: %v", i+1, err)
		}
	}
	if got := f.callCount(); got != 3 {
		t.Fatalf("三条被动回复都该出网，实际 %d 次", got)
	}
	// 一行账都不该记：否则连续聊天会悄悄吃掉主动额度。
	ctx := context.Background()
	for _, table := range []string{"active_usage", "active_rate"} {
		rows, err := db.QueryRows(ctx, `SELECT COUNT(*) AS n FROM `+table)
		if err != nil {
			t.Fatalf("查 %s: %v", table, err)
		}
		if n, _ := rows[0]["n"].(int64); n != 0 {
			t.Errorf("被动回复不该写 %s，实际 %d 行", table, n)
		}
	}
}

// 每分钟维度是 Bot 级的：换一个收件人不该换来一份新额度。
func TestActivePerMinuteLimitRefusesAndIsDetectable(t *testing.T) {
	f := &fakeAPI{}
	db := newUsageStore(t)
	s := newBudgetSender(t, f, ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 100, // 日额度放开，只让分钟维度生效
		GroupPerMin:  2,
	}, db)
	p := s.ForPlugin("digest")
	ctx := context.Background()

	for _, g := range []string{"g1", "g2"} {
		if err := p.SendActive(ctx, host.SceneGroup, g, host.Reply{Content: "x"}); err != nil {
			t.Fatalf("上限内（%s）不该失败: %v", g, err)
		}
	}
	err := p.SendActive(ctx, host.SceneGroup, "g3", host.Reply{Content: "x"})
	if err == nil {
		t.Fatal("超过每分钟上限必须拒发")
	}
	if !errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("超限应映射到 host.ErrActiveQuota（插件要能 errors.Is 判定）: %v", err)
	}
	// 与另外两类哨兵必须互斥：插件按 errors.Is 分支处置，撞值会同时进两个分支。
	if errors.Is(err, host.ErrRateLimited) || errors.Is(err, host.ErrActiveDisabled) {
		t.Fatalf("超限不该命中限流/未开启哨兵: %v", err)
	}
	for _, want := range []string{"每分钟", "group", "g3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误串 %q 缺少 %q（排障要看出是哪一维、哪个会话）", err, want)
		}
	}
	if got := f.callCount(); got != 2 {
		t.Fatalf("被拦下的那条不该出网，实际 %d 次", got)
	}
}

// 429 冷却优先于软预算：平台才是真相，冷却期内返回限流类错误而不是额度类。
func TestRateLimitCooldownBeatsSoftBudget(t *testing.T) {
	f := &fakeAPI{rateLimitedAt: 1, retryAfter: time.Minute}
	db := newUsageStore(t)
	s := newBudgetSender(t, f, ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 10,
		GroupPerMin:  2,
	}, db)
	p := s.ForPlugin("digest")
	ctx := context.Background()

	// g1 吃一次 429，进入冷却；那一格分钟额度已被平台的事实消耗掉。
	if err := p.SendActive(ctx, host.SceneGroup, "g1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrRateLimited) {
		t.Fatalf("首次应吃 429，实际 %v", err)
	}
	// g2 用掉最后一格，此刻 Bot 的分钟额度已满。
	if err := p.SendActive(ctx, host.SceneGroup, "g2", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("上限内不该失败: %v", err)
	}
	err := p.SendActive(ctx, host.SceneGroup, "g1", host.Reply{Content: "x"})
	if !errors.Is(err, host.ErrRateLimited) {
		t.Fatalf("冷却期内应返回 ErrRateLimited（429 优先于软预算），实际 %v", err)
	}
	if errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("冷却期内不该返回额度类错误: %v", err)
	}
	if got := f.callCount(); got != 2 {
		t.Fatalf("冷却期内不该出网，实际 %d 次", got)
	}
}

// 当天首次超限私聊 Bot 所有者一条，之后不再重复；通知本身不受软预算限制。
func TestQuotaExceededNotifiesOwnerOncePerDay(t *testing.T) {
	f := &fakeAPI{}
	db := newUsageStore(t)
	ctx := context.Background()
	if err := db.UpsertAdmin(ctx, store.Admin{OpenID: "owner-1", Role: store.RoleOwner}); err != nil {
		t.Fatalf("写入 owner: %v", err)
	}
	s := newBudgetSender(t, f, ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 1,
		GroupPerMin:  100,
	}, db)
	s.notices = db
	p := s.ForPlugin("digest")

	noticeCount := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, b := range f.bodies {
			if strings.Contains(b.Content, "软预算") {
				n++
			}
		}
		return n
	}

	if err := p.SendActive(ctx, host.SceneGroup, "g1", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("首条不该失败: %v", err)
	}
	if err := p.SendActive(ctx, host.SceneGroup, "g1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("第二条应超限: %v", err)
	}
	if got := noticeCount(); got != 1 {
		t.Fatalf("首次超限应通知所有者一次，实际 %d 条通知", got)
	}
	// 换一个会话再超限：当天已经通知过，不该再刷。
	if err := p.SendActive(ctx, host.SceneGroup, "g2", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("g2 首条不该失败: %v", err)
	}
	if err := p.SendActive(ctx, host.SceneGroup, "g2", host.Reply{Content: "x"}); !errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("g2 第二条应超限: %v", err)
	}
	if got := noticeCount(); got != 1 {
		t.Fatalf("当天不该重复通知，实际 %d 条", got)
	}

	// 跨天重新算一次：标记是日期，不是布尔。
	s.now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	if err := p.SendActive(ctx, host.SceneGroup, "g1", host.Reply{Content: "x"}); err != nil {
		t.Fatalf("跨天后应重新有日额度: %v", err)
	}
	if err := p.SendActive(ctx, host.SceneGroup, "g1", host.Reply{Content: "x"}); !errors.Is(err, host.ErrActiveQuota) {
		t.Fatalf("第二天第二条应超限: %v", err)
	}
	if got := noticeCount(); got != 2 {
		t.Fatalf("新的一天应再通知一次，实际 %d 条", got)
	}
}

// 16 个并发打同一个 (分钟, 场景)：不能出现"判了没扣"或"扣了没判"——
// 放行数必须恰好等于上限，账本用量也必须恰好等于上限。
func TestActivePerMinuteConcurrentDoesNotOverspend(t *testing.T) {
	f := &fakeAPI{}
	db := newUsageStore(t)
	const limit = 8
	const workers = 16
	s := newBudgetSender(t, f, ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 1000,
		GroupPerMin:  limit,
	}, db)
	p := s.ForPlugin("digest")

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := p.SendActive(context.Background(), host.SceneGroup,
				fmt.Sprintf("g%d", i), host.Reply{Content: "x"})
			switch {
			case err == nil:
				mu.Lock()
				granted++
				mu.Unlock()
			case errors.Is(err, host.ErrActiveQuota):
			default:
				t.Errorf("只应出现超限错误，实际 %v", err)
			}
		}(i)
	}
	wg.Wait()

	if granted != limit {
		t.Fatalf("%d 个并发里应恰好放行 %d 条，实际 %d", workers, limit, granted)
	}
	if got := f.callCount(); got != limit {
		t.Fatalf("出网次数应等于放行数 %d，实际 %d", limit, got)
	}
	rows, err := db.QueryRows(context.Background(),
		`SELECT used FROM active_rate WHERE scene = 'group'`)
	if err != nil {
		t.Fatalf("读分钟账本: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应只有一个分钟桶，实际 %d 行", len(rows))
	}
	if n, _ := rows[0]["used"].(int64); n != limit {
		t.Fatalf("账本用量应恰好为 %d，实际 %d", limit, n)
	}
}

// 装配层传进来的数字必须原样变成闸门：档名解析在 config，core 只认数。
func TestActivePolicyTakesNumbersFromAssembly(t *testing.T) {
	cfg := &config.Config{}
	cfg.Active.Plugins = []string{"digest"}
	cfg.Quota = map[string]config.QuotaProfile{
		config.DefaultQuotaProfile: {SingleQPM: 300, GroupQPM: 30, DailyPerPeer: 500},
	}

	got := activePolicy(cfg, &config.QuotaProfile{SingleQPM: 7, GroupQPM: 9, DailyPerPeer: 11})
	if got.SinglePerMin != 7 || got.GroupPerMin != 9 || got.DailyPerPeer != 11 {
		t.Fatalf("装配层给的数字应原样生效，实际 %+v", got)
	}
	// 没注入时按默认档推导（单 Bot 与测试的路径）。
	got = activePolicy(cfg, nil)
	if got.SinglePerMin != 300 || got.GroupPerMin != 30 || got.DailyPerPeer != 500 {
		t.Fatalf("未注入时应按默认档推导，实际 %+v", got)
	}
	// [active].daily_per_peer 是全局临时覆盖，排在档位之后。
	cfg.Active.DailyPerPeer = 2
	if got := activePolicy(cfg, nil); got.DailyPerPeer != 2 {
		t.Fatalf("[active].daily_per_peer 应覆盖档位，实际 %+v", got)
	}
}

package instance

import (
	"context"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/internal/store"
)

// 启动摘要的取值口径：#63 的推导是唯一读法（库里有 bot_state 就用它，
// 没有行时按 admins 是否为空推导），这里不许另立一套。
//
// 五个 Bot 覆盖五种来源：有行 active、有行 failing、无行无 admins（推导
// pending_claim）、无行有 admins（推导 active）、配置停用；另加一个"根本没
// 起来"的（没有 runtime、没有库）——它必须记进 failing 并单列 not_started，
// 而不是混进 active。
func TestTallyBotStatesReadsStoreNotGuesses(t *testing.T) {
	ctx := context.Background()
	disabled := false
	cfg := &config.Config{
		Store: config.Store{Path: t.TempDir() + "/qqbot.db"},
		Bots: []config.Bot{
			{AppID: "b-active", Secret: "s1"},
			{AppID: "b-failing", Secret: "s2"},
			{AppID: "b-pending", Secret: "s3"},
			{AppID: "b-claimed", Secret: "s4"},
			{AppID: "b-off", Secret: "s5", Enabled: &disabled},
			{AppID: "b-broken", Secret: "s6"},
		},
	}

	runtimes := map[string]*botRuntime{}
	for i := range cfg.Bots {
		if cfg.Bots[i].AppID == "b-broken" || cfg.Bots[i].AppID == "b-off" {
			continue // 前者没起来、后者不连接：都不给 runtime
		}
		runtimes[cfg.Bots[i].AppID] = &botRuntime{cfg: cfg.Bots[i], db: openBotStore(t, cfg, cfg.Bots[i])}
	}

	if err := core.SaveBotState(ctx, runtimes["b-active"].db, core.BotStateActive); err != nil {
		t.Fatalf("预置 b-active 状态: %v", err)
	}
	if err := core.SaveBotState(ctx, runtimes["b-failing"].db, core.BotStateFailing); err != nil {
		t.Fatalf("预置 b-failing 状态: %v", err)
	}
	// b-claimed 刻意不写状态行，只放一条 owner：推导出来的 active 才是
	// "已认领但还没迁移过状态"的真实形态（#63 明确推导不落库）。
	if err := runtimes["b-claimed"].db.UpsertAdmin(ctx, store.Admin{
		OpenID: "u-owner", Role: store.RoleOwner, GrantedBy: "test",
	}); err != nil {
		t.Fatalf("预置 b-claimed 的 admins: %v", err)
	}

	got := tallyBotStates(ctx, quietLogger(), cfg.Bots, runtimes)
	want := botStateTally{
		Total: 6, Active: 2, PendingClaim: 1, Failing: 2, Suspended: 1, NotStarted: 1,
	}
	if got != want {
		t.Fatalf("汇总 = %+v，期望 %+v", got, want)
	}

	// 四个状态计数之和必须等于总数：运维对着这一行做加减，凑不上就说明
	// 有 Bot 掉进了没被计入的分支。
	if sum := got.Active + got.PendingClaim + got.Failing + got.Suspended; sum != got.Total {
		t.Errorf("四个状态之和 %d 与总数 %d 不等", sum, got.Total)
	}

	const wantText = "共 6 个 Bot：2 个 active、1 个 pending_claim、2 个 failing、1 个 suspended"
	if txt := got.String(); txt != wantText {
		t.Errorf("摘要正文 = %q，期望 %q", txt, wantText)
	}
}

// 一个 Bot 的库读不出来（例如迁移没跑完、文件被换成了空库）时按 failing 计入，
// 不打出一个会让人以为是 active 的数。
func TestTallyBotStatesCountsUnreadableStoreAsFailing(t *testing.T) {
	cfg := &config.Config{
		Store: config.Store{Path: t.TempDir() + "/qqbot.db"},
		Bots:  []config.Bot{{AppID: "b-bad", Secret: "s"}},
	}
	// 刻意不迁移：bot_state 表不存在，LoadBotState 必然报错。
	db, err := store.Open(cfg.BotStorePath(cfg.Bots[0]))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runtimes := map[string]*botRuntime{"b-bad": {cfg: cfg.Bots[0], db: db}}

	got := tallyBotStates(context.Background(), quietLogger(), cfg.Bots, runtimes)
	if got.Failing != 1 || got.Active != 0 || got.PendingClaim != 0 {
		t.Fatalf("读库失败应记 failing，实际 %+v", got)
	}
	if !strings.Contains(got.String(), "1 个 failing") {
		t.Errorf("摘要正文没把读库失败算进去: %q", got.String())
	}
}

// openBotStore 开一份该 Bot 的库（已迁移），测试结束自动关。
func openBotStore(t *testing.T, cfg *config.Config, b config.Bot) *store.DB {
	t.Helper()
	db, err := store.Open(cfg.BotStorePath(b))
	if err != nil {
		t.Fatalf("打开 %s 的库: %v", b.AppID, err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		_ = db.Close()
		t.Fatalf("迁移 %s 的库: %v", b.AppID, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

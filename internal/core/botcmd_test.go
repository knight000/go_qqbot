package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// fakeHot 是装配层的替身：只记"被要求做了什么"，不真的开库、连网关。
//
// 用替身而不是真容器：core 这边的验收全是**命令面**（谁被拒、回了什么、
// 调没调到、确认码对不对），与解包 SQLite 无关。
type fakeHot struct {
	views []BotView

	sum      BotReloadSummary
	reloadEr error

	reloadAllSelf string

	reloadOneAppid string
	reloadOneSelf  string
	reloadOneErr   error

	stats    store.ResetCounts
	statsErr error

	purged   []string
	purgeErr error
}

func (f *fakeHot) Snapshot(context.Context) ([]BotView, error) { return f.views, nil }

func (f *fakeHot) ReloadAll(_ context.Context, self string) (BotReloadSummary, error) {
	f.reloadAllSelf = self
	return f.sum, f.reloadEr
}

func (f *fakeHot) ReloadOne(_ context.Context, self, appid string) error {
	f.reloadOneSelf, f.reloadOneAppid = self, appid
	return f.reloadOneErr
}

func (f *fakeHot) PurgeStats(context.Context, string) (store.ResetCounts, error) {
	return f.stats, f.statsErr
}

func (f *fakeHot) Purge(_ context.Context, appid string) (store.ResetCounts, int, error) {
	f.purged = append(f.purged, appid)
	return f.stats, 0, f.purgeErr
}

// newBotCore 造一个"配了 Core 所有者、装了热操作"的核心。
func newBotCore(t *testing.T, hot BotHotService) (*Core, *fakeAPI) {
	t.Helper()
	c, f := newAdminCore(t)
	c.cfg = &config.Config{Core: config.Core{Owners: []string{"core-1"}}}
	c.botID = "102000001"
	c.hot = hot
	return c, f
}

func botCall(t *testing.T, c *Core, f *fakeAPI, content, sender string) string {
	t.Helper()
	ev := adminEvent(content, sender, "", host.SceneC2C, sender)
	if !c.handleReserved(context.Background(), ev) {
		t.Fatalf("%q 应被核心消费", content)
	}
	return lastReply(t, f)
}

// /bot 是进程级指令：只认 [core].owners，与库里的 admins 无关。
func TestBotCommandsRejectNonCoreOwner(t *testing.T) {
	hot := &fakeHot{}
	c, f := newBotCore(t, hot)
	// 库里的 owner 也不行：两套角色不相交（ADR-0008 决策 9）。
	seedAdmin(t, c, "bot-owner", "", store.RoleOwner)

	for _, cmd := range []string{"/bot list", "/bot reload", "/bot remove 102000002 --purge"} {
		got := botCall(t, c, f, cmd, "bot-owner")
		if !strings.Contains(got, "仅 Core 所有者") {
			t.Fatalf("%s 非 Core 所有者应被拒绝，实际: %q", cmd, got)
		}
	}
	if hot.reloadAllSelf != "" || len(hot.purged) != 0 {
		t.Fatal("被拒绝的指令不该触达装配层")
	}
}

// 没配 [core].owners 时一律拒绝：空名单最坏的默认值是"谁都能管进程"。
func TestBotRejectsWhenNoCoreOwnerConfigured(t *testing.T) {
	c, f := newAdminCore(t) // cfg 是空的 &config.Config{}
	c.botID = "102000001"
	c.hot = &fakeHot{}

	if got := botCall(t, c, f, "/bot list", "anyone"); !strings.Contains(got, "仅 Core 所有者") {
		t.Fatalf("没有 owner 配置时应拒绝，实际: %q", got)
	}
}

func TestBotWithoutServiceSaysSo(t *testing.T) {
	c, f := newBotCore(t, nil)

	got := botCall(t, c, f, "/bot list", "core-1")
	if !strings.Contains(got, "热操作未装配") {
		t.Fatalf("未装配时应如实说明，实际: %q", got)
	}
}

// 一行一个 Bot（appid / 认证类型 / 状态 / 是否已认领 / 配置 enabled），末行汇总。
func TestBotListRendersEachBotAndSummary(t *testing.T) {
	hot := &fakeHot{views: []BotView{
		{AppID: "102000001", CertType: "personal", State: "active", Claimed: true, Enabled: true, Running: true},
		{AppID: "102000002", CertType: "enterprise", State: "pending_claim", Enabled: true, Running: true},
		{AppID: "102000003", CertType: "personal", State: "suspended",
			SuspendReason: SuspendReasonConfigGone, Enabled: false},
	}}
	c, f := newBotCore(t, hot)

	got := botCall(t, c, f, "/bot list", "core-1")
	for _, want := range []string{
		"Bot 列表（3 个）",
		"102000001  personal  active  已认领  enabled=true",
		"102000002  enterprise  pending_claim  未认领  enabled=true",
		"102000003  personal  suspended(config_gone)  未认领  enabled=false  未连接",
		"汇总：连接 2、已认领 1、failing 0、suspended 1（其中配置外 1）",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("列表输出缺少 %q，实际:\n%s", want, got)
		}
	}
}

// 摘要必须是一句能对上号的话：「新增 1、重启 1、停用 1」。
func TestBotReloadReportsSummary(t *testing.T) {
	hot := &fakeHot{sum: BotReloadSummary{Added: 1, Restarted: 1, Stopped: 1, Unchanged: 2}}
	c, f := newBotCore(t, hot)

	got := botCall(t, c, f, "/bot reload", "core-1")
	if !strings.Contains(got, "新增 1、重启 1、停用 1") {
		t.Fatalf("摘要应带三类计数，实际: %q", got)
	}
	if !strings.Contains(got, "其余 2 个未变") {
		t.Errorf("未变的数量也应说出来，实际: %q", got)
	}
}

// 发指令的那个 Bot 不能被自己这次 reload 停掉：会丢掉这条回复。
func TestBotReloadMentionsDeferredSelf(t *testing.T) {
	hot := &fakeHot{sum: BotReloadSummary{Added: 1, Deferred: []string{"102000001"}}}
	c, f := newBotCore(t, hot)

	got := botCall(t, c, f, "/bot reload", "core-1")
	if !strings.Contains(got, "本 Bot 的变更未执行") {
		t.Fatalf("自己的变更必须被明确告知，实际: %q", got)
	}
	if hot.reloadAllSelf != "102000001" {
		t.Fatalf("装配层应收到发起者 appid，实际 %q", hot.reloadAllSelf)
	}
}

func TestBotReloadOneTargetsOnlyThatBot(t *testing.T) {
	hot := &fakeHot{}
	c, f := newBotCore(t, hot)

	got := botCall(t, c, f, "/bot reload 102000002", "core-1")
	if hot.reloadOneAppid != "102000002" {
		t.Fatalf("应只重连 102000002，实际 %q", hot.reloadOneAppid)
	}
	if !strings.Contains(got, "已重连 102000002") {
		t.Fatalf("回复应点名重连了谁，实际: %q", got)
	}
}

func TestBotReloadOneOnSelfIsRefused(t *testing.T) {
	hot := &fakeHot{reloadOneErr: ErrBotSelfOp}
	c, f := newBotCore(t, hot)

	got := botCall(t, c, f, "/bot reload 102000001", "core-1")
	if !strings.Contains(got, "不能重连 102000001") || !strings.Contains(got, "/reload") {
		t.Fatalf("拒绝时要给出可行路径，实际: %q", got)
	}
}

// /bot remove 只支持 --purge：不带它的写法会让人以为"删配置条目就够了"。
func TestBotRemoveRequiresPurgeFlag(t *testing.T) {
	hot := &fakeHot{}
	c, f := newBotCore(t, hot)

	got := botCall(t, c, f, "/bot remove 102000002", "core-1")
	if !strings.Contains(got, "--purge") {
		t.Fatalf("应指向 --purge 的用法，实际: %q", got)
	}
	if len(hot.purged) != 0 {
		t.Fatal("不该执行任何清理")
	}
}

// 前置校验：配置里还有这个 appid 时拒绝，并说清"先删条目 + reload"。
func TestBotRemoveRejectedWhileStillConfigured(t *testing.T) {
	hot := &fakeHot{statsErr: ErrBotStillConfigured}
	c, f := newBotCore(t, hot)

	got := botCall(t, c, f, "/bot remove 102000002 --purge", "core-1")
	if !strings.Contains(got, "仍在配置里") || !strings.Contains(got, "/bot reload") {
		t.Fatalf("应拒绝并给出操作顺序，实际: %q", got)
	}
	if len(hot.purged) != 0 {
		t.Fatal("配置里还在时不能落盘清理")
	}
}

// 两步确认：先发码预览，再用码执行；审计记 done。
func TestBotRemoveNeedsConfirmCodeThenPurges(t *testing.T) {
	hot := &fakeHot{stats: store.ResetCounts{Messages: 7, MediaRows: 2}}
	c, f := newBotCore(t, hot)

	first := botCall(t, c, f, "/bot remove 102000002 --purge", "core-1")
	if !strings.Contains(first, "不可恢复") || !strings.Contains(first, "确认") {
		t.Fatalf("第一次调用应只预览并发码，实际: %q", first)
	}
	code := digitToken(t, first)
	if len(hot.purged) != 0 {
		t.Fatal("发码阶段绝不能执行")
	}

	second := botCall(t, c, f, "/bot remove 102000002 --purge "+code, "core-1")
	if len(hot.purged) != 1 || hot.purged[0] != "102000002" {
		t.Fatalf("确认后应执行一次清理，实际 %v", hot.purged)
	}
	if !strings.Contains(second, "已清理 102000002") || !strings.Contains(second, "消息 7 条") {
		t.Fatalf("结果应带清理账，实际: %q", second)
	}

	recs, err := c.store.RecentResets(context.Background(), 5)
	if err != nil || len(recs) != 1 {
		t.Fatalf("应有一条审计记录: %+v err=%v", recs, err)
	}
	if recs[0].Level != botPurgeLevel || recs[0].Status != store.ResetStatusDone {
		t.Fatalf("审计应记 %s/done，实际 %s/%s", botPurgeLevel, recs[0].Level, recs[0].Status)
	}
}

func TestBotRemoveRejectsWrongCode(t *testing.T) {
	hot := &fakeHot{}
	c, f := newBotCore(t, hot)

	first := botCall(t, c, f, "/bot remove 102000002 --purge", "core-1")
	code := digitToken(t, first)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}

	got := botCall(t, c, f, "/bot remove 102000002 --purge "+wrong, "core-1")
	if !strings.Contains(got, "确认码不正确") {
		t.Fatalf("错码应被拒，实际: %q", got)
	}
	if len(hot.purged) != 0 {
		t.Fatal("错码不能执行清理")
	}
}

// 发码到确认之间配置又被加回去：执行前必须再判一次（这里是更早的那道闸
// 先命中——带码的调用同样先过前置校验，所以连确认码都不会被消费）。
func TestBotRemoveRechecksConfigBeforePurge(t *testing.T) {
	hot := &fakeHot{}
	c, f := newBotCore(t, hot)

	first := botCall(t, c, f, "/bot remove 102000002 --purge", "core-1")
	code := digitToken(t, first)

	hot.statsErr = ErrBotStillConfigured
	got := botCall(t, c, f, "/bot remove 102000002 --purge "+code, "core-1")
	if !strings.Contains(got, "仍在配置里") {
		t.Fatalf("配置回退时应中止，实际: %q", got)
	}
	if len(hot.purged) != 0 {
		t.Fatal("配置里又出现时不能清库")
	}
}

func TestBotRemoveRefusesSelf(t *testing.T) {
	hot := &fakeHot{}
	c, f := newBotCore(t, hot)

	got := botCall(t, c, f, "/bot remove 102000001 --purge", "core-1")
	if !strings.Contains(got, "不能") {
		t.Fatalf("删自己应被拒绝，实际: %q", got)
	}
}

func TestBotUnknownSubcommandShowsUsage(t *testing.T) {
	c, f := newBotCore(t, &fakeHot{})
	got := botCall(t, c, f, "/bot frobnicate", "core-1")
	if !strings.Contains(got, "未知的子命令") || !strings.Contains(got, "/bot list") {
		t.Fatalf("未知子命令应给用法，实际: %q", got)
	}
}

// 装配层的失败要原样说出来，不能吞成一句"失败了"。
func TestBotReloadSurfacesConfigError(t *testing.T) {
	hot := &fakeHot{reloadEr: errors.New("解析配置失败: 第 3 行")}
	c, f := newBotCore(t, hot)

	got := botCall(t, c, f, "/bot reload", "core-1")
	if !strings.Contains(got, "第 3 行") || !strings.Contains(got, "未改动任何 Bot") {
		t.Fatalf("应带上原因并说明没有改动，实际: %q", got)
	}
}

// digitToken 从回复里取出 6 位确认码。
func digitToken(t *testing.T, text string) string {
	t.Helper()
	for _, f := range strings.Fields(text) {
		if isResetCode(f) {
			return f
		}
	}
	t.Fatalf("回复里没有 6 位确认码: %q", text)
	return ""
}

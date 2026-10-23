package core

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

type fakeReconnector struct {
	calls int
	err   error
}

func (f *fakeReconnector) Reconnect() error {
	f.calls++
	return f.err
}

// resetCall 走完整入口：既验证 /reset 被核心独占，也顺带覆盖指令解析。
func resetCall(t *testing.T, c *Core, f *fakeAPI, ctx context.Context, content, sender string, scene host.Scene, peer string) string {
	ev := adminEvent(content, sender, "", scene, peer)
	if !c.handleReserved(ctx, ev) {
		return "<未被核心消费>"
	}
	return lastReply(t, f)
}

func TestResetRejectsNonAdmin(t *testing.T) {
	c, f := newAdminCore(t)
	ctx := context.Background()

	got := resetCall(t, c, f, ctx, "/reset status", "stranger", host.SceneGroup, "g1")
	if !strings.Contains(got, "仅管理员") {
		t.Fatalf("非管理员应被拒绝，实际: %q", got)
	}
}

func TestResetHelpWithoutArgs(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)

	got := resetCall(t, c, f, context.Background(), "/reset", "admin-1", host.SceneGroup, "g1")
	for _, want := range []string{"reconnect", "session", "data", "all", "确认码"} {
		if !strings.Contains(got, want) {
			t.Errorf("帮助应包含 %q，实际: %q", want, got)
		}
	}
}

func TestResetUnknownLevel(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)

	got := resetCall(t, c, f, context.Background(), "/reset everything", "admin-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "未知的重置级别") {
		t.Fatalf("未知级别应报错，实际: %q", got)
	}
}

// R0：只有所有者能重连，且它不该碰任何数据。
func TestResetReconnectOwnerOnly(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)
	rc := &fakeReconnector{}
	c.SetReconnector(rc)
	ctx := context.Background()

	got := resetCall(t, c, f, ctx, "/reset reconnect", "admin-1", host.SceneC2C, "admin-1")
	if !strings.Contains(got, "仅 Bot 所有者") {
		t.Fatalf("admin 不应能重连，实际: %q", got)
	}
	if rc.calls != 0 {
		t.Fatal("被拒绝的请求不应触达网关")
	}

	got = resetCall(t, c, f, ctx, "/reset r0", "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "已请求网关立即重连") {
		t.Fatalf("owner 应能重连，实际: %q", got)
	}
	if rc.calls != 1 {
		t.Fatalf("应调用一次重连，实际 %d", rc.calls)
	}
}

func TestResetReconnectGatewayNotReady(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	c.SetReconnector(&fakeReconnector{err: errors.New("网关当前未连接")})

	got := resetCall(t, c, f, context.Background(), "/reset reconnect", "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "网关当前未连接") {
		t.Fatalf("网关未连接时应把原因说清楚，实际: %q", got)
	}
	// 失败也要留痕：审计记录的是"尝试过"，不只是"成功过"。
	recs, err := c.store.RecentResets(context.Background(), 5)
	if err != nil || len(recs) != 1 || !strings.Contains(recs[0].Detail, "失败") {
		t.Fatalf("失败的尝试也应进审计: %+v err=%v", recs, err)
	}
	// 记录的是"失败"而不是"已执行"：审计不能谎报。
	if recs[0].Status != store.ResetStatusFailed {
		t.Fatalf("失败的状态应为 %q，实际 %q", store.ResetStatusFailed, recs[0].Status)
	}
}

// P2-3：失败的重置不能占用冷却窗口，否则 R3 失败一次就锁 24 小时。
func TestResetFailureDoesNotConsumeCooldown(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	c.SetReconnector(&fakeReconnector{err: errors.New("网关当前未连接")})
	ctx := context.Background()

	got := resetCall(t, c, f, ctx, "/reset reconnect", "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "重置失败") {
		t.Fatalf("应报告失败，实际: %q", got)
	}
	if !strings.Contains(got, "不计入冷却") {
		t.Errorf("失败回复应说明冷却没被占用，实际: %q", got)
	}
	if _, ok, err := c.store.LastResetDoneAt(ctx, store.ResetLevelReconnect, store.ResetScopeGlobal); err != nil || ok {
		t.Fatalf("失败不得写入冷却起点: ok=%v err=%v", ok, err)
	}

	// 状态页不能把失败说成"已执行"，也不能说冷却中。
	got = resetCall(t, c, f, ctx, "/reset status", "owner-1", host.SceneC2C, "owner-1")
	if strings.Contains(got, "已执行") {
		t.Errorf("失败不该显示成已执行，实际: %q", got)
	}
	if !strings.Contains(got, "失败") {
		t.Errorf("状态页应明确显示失败，实际: %q", got)
	}

	// 失败后可立即重试：仍然执行（再次失败），而不是被冷却挡住。
	got = resetCall(t, c, f, ctx, "/reset reconnect", "owner-1", host.SceneC2C, "owner-1")
	if strings.Contains(got, "冷却中") {
		t.Fatalf("失败不应触发冷却，实际: %q", got)
	}
	if !strings.Contains(got, "重置失败") {
		t.Fatalf("应立即重试，实际: %q", got)
	}
}

// 有确认码的路径（R2/R3）失败时，pending 记录要关成 failed，不是 done。
// 用 SQLite 触发器让清理阶段的 DELETE 报错——这是"库层面失败"的真实形态。
func TestResetConfirmedFailureRecordedAsFailed(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	ctx := context.Background()

	if err := c.store.InsertMessage(ctx, store.Message{
		EventID: "e1", Scene: "group", PeerOpenID: "g1", Content: "hi",
	}); err != nil {
		t.Fatal(err)
	}

	resetCall(t, c, f, ctx, "/reset data", "owner-1", host.SceneGroup, "g1")
	rec, ok, err := c.store.PendingReset(ctx, "owner-1", "g1", store.ResetLevelData)
	if err != nil || !ok {
		t.Fatalf("应生成确认码: ok=%v err=%v", ok, err)
	}

	// 让删消息这一步必失败，模拟执行中途的库错误。
	sqldb, err := sql.Open("sqlite", "file:"+filepath.ToSlash(c.store.Path())+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	if _, err := sqldb.ExecContext(ctx,
		`CREATE TRIGGER refuse_message_delete BEFORE DELETE ON messages
		 BEGIN SELECT RAISE(ABORT, '模拟磁盘故障'); END`); err != nil {
		t.Fatalf("注入故障触发器: %v", err)
	}

	got := resetCall(t, c, f, ctx, "/reset data "+rec.Code, "owner-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "重置失败") || !strings.Contains(got, "模拟磁盘故障") {
		t.Fatalf("应把失败原因回给用户，实际: %q", got)
	}

	// 数据没删掉（事务回滚），但审计如实写了失败。
	if stats, _ := c.store.PeerStats(ctx, "g1"); stats.Messages != 1 {
		t.Fatalf("失败的清理不应留下半截状态，剩余消息 %d", stats.Messages)
	}
	recs, err := c.store.RecentResets(ctx, 5)
	if err != nil || len(recs) == 0 {
		t.Fatalf("应留下审计: %+v err=%v", recs, err)
	}
	if recs[0].Status != store.ResetStatusFailed {
		t.Fatalf("失败的状态应为 %q，实际 %q", store.ResetStatusFailed, recs[0].Status)
	}
	if _, ok, _ := c.store.LastResetDoneAt(ctx, store.ResetLevelData, "g1"); ok {
		t.Fatal("失败不得写入冷却起点")
	}

	// 状态页据此显示失败，且该级别仍可执行。
	got = resetCall(t, c, f, ctx, "/reset status", "owner-1", host.SceneGroup, "g1")
	if strings.Contains(got, "已执行") {
		t.Errorf("失败不该显示成已执行，实际: %q", got)
	}
	if !strings.Contains(got, "失败") {
		t.Errorf("状态页应明确显示失败，实际: %q", got)
	}
}

// R1：只清当前会话的插件状态，插件级全局键与别的会话都不受影响。
func TestResetSessionScoped(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)
	ctx := context.Background()

	for _, kv := range []struct{ peer, val string }{
		{"", "global"}, {"g1", "one"}, {"g2", "two"},
	} {
		if err := c.store.KVSet(ctx, "echo", kv.peer, "state", []byte(kv.val), 0); err != nil {
			t.Fatal(err)
		}
	}

	got := resetCall(t, c, f, ctx, "/reset session", "admin-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "会话状态 1 条") {
		t.Fatalf("应报告清掉 1 条，实际: %q", got)
	}
	if _, ok, _ := c.store.KVGet(ctx, "echo", "g1", "state"); ok {
		t.Fatal("g1 的状态应已清空")
	}
	if _, ok, _ := c.store.KVGet(ctx, "echo", "g2", "state"); !ok {
		t.Fatal("g2 不应被波及")
	}
	if _, ok, _ := c.store.KVGet(ctx, "echo", "", "state"); !ok {
		t.Fatal("插件级全局键不应被 R1 清掉")
	}
}

// R2 第一次只发码预览，一行都不许删——这是"误触不会造成损失"的全部保证。
func TestResetDataRequiresConfirm(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)
	ctx := context.Background()

	for _, id := range []string{"e1", "e2"} {
		if err := c.store.InsertMessage(ctx, store.Message{
			EventID: id, Scene: "group", PeerOpenID: "g1", Content: "hi",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 任务执行历史也属于该群的业务数据，预览与执行都必须算进去。
	for _, ts := range []int64{1, 2} {
		if err := c.store.RecordJobRun(ctx, "digest:daily", "g1", ts, ts+1, "ok", ""); err != nil {
			t.Fatal(err)
		}
	}

	got := resetCall(t, c, f, ctx, "/reset data", "admin-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "当前消息 2 条") || !strings.Contains(got, "任务记录 2 条") ||
		!strings.Contains(got, "确认") {
		t.Fatalf("预览应给出数量与确认提示，实际: %q", got)
	}
	if stats, _ := c.store.PeerStats(ctx, "g1"); stats.Messages != 2 || stats.JobRuns != 2 {
		t.Fatalf("预览阶段不得删除任何数据，实际剩 消息=%d 任务记录=%d", stats.Messages, stats.JobRuns)
	}

	rec, ok, err := c.store.PendingReset(ctx, "admin-1", "g1", store.ResetLevelData)
	if err != nil || !ok {
		t.Fatalf("应留下待确认记录: ok=%v err=%v", ok, err)
	}

	got = resetCall(t, c, f, ctx, "/reset data "+rec.Code, "admin-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "已清空本会话数据") || !strings.Contains(got, "任务记录 2 条") {
		t.Fatalf("确认后应执行，实际: %q", got)
	}
	if stats, _ := c.store.PeerStats(ctx, "g1"); stats.Messages != 0 || stats.JobRuns != 0 {
		t.Fatalf("确认后消息与任务记录应清空，实际剩 消息=%d 任务记录=%d",
			stats.Messages, stats.JobRuns)
	}

	// 同一个码不能再用一次。
	got = resetCall(t, c, f, ctx, "/reset data "+rec.Code, "admin-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "没有待确认的请求") {
		t.Fatalf("确认码应一次性，实际: %q", got)
	}
}

// 确认码绑定 actor + peer + level：换个会话、换个人、换个级别都用不了。
func TestResetCodeBoundToActorPeerLevel(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	seedAdmin(t, c, "admin-2", "", store.RoleAdmin)
	ctx := context.Background()

	resetCall(t, c, f, ctx, "/reset data", "owner-1", host.SceneGroup, "g1")
	rec, ok, _ := c.store.PendingReset(ctx, "owner-1", "g1", store.ResetLevelData)
	if !ok {
		t.Fatal("应生成确认码")
	}

	cases := []struct {
		name             string
		sender, peer, cm string
	}{
		{"换会话", "owner-1", "g2", "/reset data " + rec.Code},
		{"换人", "admin-2", "g1", "/reset data " + rec.Code},
		{"换级别", "owner-1", "g1", "/reset all " + rec.Code},
	}
	for _, tc := range cases {
		got := resetCall(t, c, f, ctx, tc.cm, tc.sender, host.SceneGroup, tc.peer)
		if !strings.Contains(got, "没有待确认的请求") {
			t.Errorf("%s 应被拒绝，实际: %q", tc.name, got)
		}
	}
}

func TestResetCooldownBlocksRepeat(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)
	ctx := context.Background()

	resetCall(t, c, f, ctx, "/reset session", "admin-1", host.SceneGroup, "g1")
	got := resetCall(t, c, f, ctx, "/reset session", "admin-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "冷却中") {
		t.Fatalf("第二次应被冷却挡住，实际: %q", got)
	}

	// 冷却按 (级别, peer)：换个群不受影响。
	got = resetCall(t, c, f, ctx, "/reset session", "admin-1", host.SceneGroup, "g2")
	if strings.Contains(got, "冷却中") {
		t.Fatalf("另一个群不该受 g1 的冷却影响，实际: %q", got)
	}
}

// R3 要短语：确认码对了但没有「确认」二字，仍然不执行。
func TestResetAllRequiresPhrase(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	ctx := context.Background()
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeAllow); err != nil {
		t.Fatal(err)
	}
	if err := c.store.InsertMessage(ctx, store.Message{
		EventID: "e1", Scene: "group", PeerOpenID: "g1", Content: "hi",
	}); err != nil {
		t.Fatal(err)
	}

	got := resetCall(t, c, f, ctx, "/reset all", "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "当前消息 1 条") || !strings.Contains(got, "保留：管理员、群名单") {
		t.Fatalf("预览应说明清理范围与保留项，实际: %q", got)
	}
	rec, ok, _ := c.store.PendingReset(ctx, "owner-1", store.ResetScopeGlobal, store.ResetLevelAll)
	if !ok {
		t.Fatal("应生成确认码")
	}

	got = resetCall(t, c, f, ctx, "/reset all "+rec.Code, "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "还需要在末尾原样输入") {
		t.Fatalf("缺短语时应拒绝，实际: %q", got)
	}
	if stats, _ := c.store.DataStats(ctx); stats.Messages != 1 {
		t.Fatal("缺短语时不得执行")
	}

	got = resetCall(t, c, f, ctx, "/reset all "+rec.Code+" 确认", "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "已清空全部业务数据") {
		t.Fatalf("带短语后应执行，实际: %q", got)
	}
	if stats, _ := c.store.DataStats(ctx); stats.Total() != 0 {
		t.Fatalf("业务表应清空: %+v", stats)
	}
	// 群名单必须活下来，否则重置等于把所有群静默拉黑。
	if g, ok, _ := c.store.GetGroup(ctx, "g1"); !ok || g.Mode != store.GroupModeAllow {
		t.Fatalf("R3 不得清群名单: %+v ok=%v", g, ok)
	}
}

func TestResetAllAdminRejected(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "admin-1", "", store.RoleAdmin)

	got := resetCall(t, c, f, context.Background(), "/reset all", "admin-1", host.SceneC2C, "admin-1")
	if !strings.Contains(got, "仅 Bot 所有者") {
		t.Fatalf("R3 应仅限所有者，实际: %q", got)
	}
}

func TestResetStatusShowsLevels(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	ctx := context.Background()

	resetCall(t, c, f, ctx, "/reset session", "owner-1", host.SceneGroup, "g1")
	got := resetCall(t, c, f, ctx, "/reset status", "owner-1", host.SceneGroup, "g1")
	for _, want := range []string{"R0", "R1", "R2", "R3", "冷却"} {
		if !strings.Contains(got, want) {
			t.Errorf("状态里应有 %q，实际: %q", want, got)
		}
	}
	if !strings.Contains(got, "已执行") {
		t.Errorf("最近记录应显示已执行，实际: %q", got)
	}
}

// R2 也接受显式 openid：私聊里清理某个群。
func TestResetDataForExplicitGroup(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	ctx := context.Background()

	const gid = "0123456789abcdef0123456789abcdef"
	if err := c.store.InsertMessage(ctx, store.Message{
		EventID: "e1", Scene: "group", PeerOpenID: gid, Content: "hi",
	}); err != nil {
		t.Fatal(err)
	}

	got := resetCall(t, c, f, ctx, "/reset data "+gid, "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "当前消息 1 条") {
		t.Fatalf("应作用于显式指定的群，实际: %q", got)
	}
	rec, ok, _ := c.store.PendingReset(ctx, "owner-1", gid, store.ResetLevelData)
	if !ok {
		t.Fatal("待确认记录应绑定到该群")
	}
	got = resetCall(t, c, f, ctx, "/reset data "+gid+" "+rec.Code, "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "已清空本会话数据") {
		t.Fatalf("确认后应执行，实际: %q", got)
	}
	if stats, _ := c.store.PeerStats(ctx, gid); stats.Messages != 0 {
		t.Fatal("该群消息应已清空")
	}
}

func TestResetGlobalLevelRejectsTarget(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)

	const gid = "0123456789abcdef0123456789abcdef"
	got := resetCall(t, c, f, context.Background(), "/reset all "+gid, "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "全局操作") {
		t.Fatalf("全局级别不该接受目标会话，实际: %q", got)
	}
}

// --- 解析与文件删除 ---

func TestSplitResetArgs(t *testing.T) {
	const gid = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		args                 []string
		target, code, phrase string
	}{
		{nil, "", "", ""},
		{[]string{"123456"}, "", "123456", ""},
		{[]string{gid}, gid, "", ""},
		{[]string{gid, "123456"}, gid, "123456", ""},
		{[]string{"123456", "确认"}, "", "123456", "确认"},
		// 顺序无关：形态本身就足以区分三者。
		{[]string{"确认", "123456"}, "", "123456", "确认"},
	}
	for _, tc := range cases {
		target, code, phrase := splitResetArgs(tc.args)
		if target != tc.target || code != tc.code || phrase != tc.phrase {
			t.Errorf("splitResetArgs(%v) = (%q,%q,%q)，期望 (%q,%q,%q)",
				tc.args, target, code, phrase, tc.target, tc.code, tc.phrase)
		}
	}
}

// 配了 [media].dir 之后，库里的 path 只能删该目录内的。
// 否则一条被写坏的记录就变成一次任意文件删除。
func TestRemoveMediaFilesStaysInsideDir(t *testing.T) {
	c, _ := newAdminCore(t)
	root := t.TempDir()
	c.cfg = &config.Config{Media: config.Media{Dir: root}}

	inside := filepath.Join(root, "a.jpg")
	outside := filepath.Join(t.TempDir(), "b.jpg")
	for _, p := range []string{inside, outside} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed := c.removeMediaFiles([]string{inside, outside})
	if removed != 1 {
		t.Fatalf("应只删目录内的 1 个，实际 %d", removed)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Error("目录内的文件应已删除")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("目录外的文件不得被删除")
	}
}

func TestHumanRemain(t *testing.T) {
	cases := []struct {
		d    string
		want string
	}{
		{"30s", "30 秒"},
		{"90s", "1 分 30 秒"},
		{"2m", "2 分钟"},
		{"90m", "1 小时 30 分"},
		{"24h", "24 小时"},
	}
	for _, tc := range cases {
		d, err := time.ParseDuration(tc.d)
		if err != nil {
			t.Fatal(err)
		}
		if got := humanRemain(d); got != tc.want {
			t.Errorf("humanRemain(%s) = %q，期望 %q", tc.d, got, tc.want)
		}
	}
}

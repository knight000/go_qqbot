package core

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// newStateCore 造一个"库里还没有状态行"的 Core：闸门会按 admins 为空推导成
// pending_claim。要"已认领"形态的用例走 newGateCore（它预置 active）。
func newStateCore(t *testing.T, plugins ...host.Plugin) (*Core, *fakeAPI) {
	t.Helper()
	db := newUsageStore(t)
	f := &fakeAPI{}
	log := slog.New(slog.DiscardHandler)
	router, err := NewRouter(plugins)
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	sender := NewSender(f, log, ActivePolicy{Enabled: true}, nil, time.Local)
	c := &Core{
		store:     db,
		cfg:       &config.Config{},
		log:       log,
		api:       f,
		sender:    sender,
		router:    router,
		dispatch:  NewDispatcher(router, sender, log, time.Second),
		startedAt: time.Now(),
		restart:   make(chan RestartRequest, 1),
		dropped:   make(map[BotState]int64, 4),
	}
	return c, f
}

func stateOf(t *testing.T, c *Core) BotState {
	t.Helper()
	st, err := c.BotState(context.Background())
	if err != nil {
		t.Fatalf("读状态: %v", err)
	}
	return st
}

// pending_claim 的 Bot 收到普通消息时必须"库里零行"：不落 messages、不进
// 幂等表、不到插件。这条同时钉住"闸门在去重之前"——去重若先跑，
// processed_events 一定有一行。
func TestPendingClaimDropsWithoutTouchingStore(t *testing.T) {
	probe := &probePlugin{manifest: host.Manifest{
		Name: "probe", APIVersion: host.APIVersion, Intents: []string{"message"},
	}}
	c, f := newStateCore(t, probe)
	ctx := context.Background()

	if st := stateOf(t, c); st != BotStatePendingClaim {
		t.Fatalf("admins 为空应推导成 pending_claim，实际 %q", st)
	}

	ev := gateEvent("hello", "u1", "g1")
	c.Handle(ctx, ev)

	if n := countRows(t, c.store, "messages"); n != 0 {
		t.Errorf("pending_claim 不得落库，messages 实际 %d 行", n)
	}
	if n := countRows(t, c.store, "processed_events"); n != 0 {
		t.Errorf("pending_claim 不得进幂等表，processed_events 实际 %d 行", n)
	}
	if n := probe.hits.Load(); n != 0 {
		t.Errorf("pending_claim 不得触发插件，实际 %d 次", n)
	}
	if got := c.DroppedByState()[BotStatePendingClaim]; got != 1 {
		t.Errorf("丢弃计数应按状态累计，实际 %d", got)
	}

	// /claim 必须穿过去：没有令牌时回一句"失效"，说明它走到了核心指令分支。
	c.Handle(ctx, adminEvent("/claim abc", "u1", "", host.SceneC2C, "u1"))
	if _, ok := f.lastBody(); !ok {
		t.Fatal("/claim 是 pending_claim 唯一放行的指令，应产生一条回复")
	}
}

// suspended / failing 连 /claim 都不放行：状态是人工/退避维度的结论，
// 不该被一条消息推翻。
func TestSuspendedAndFailingDropEverything(t *testing.T) {
	for _, st := range []BotState{BotStateSuspended, BotStateFailing} {
		t.Run(string(st), func(t *testing.T) {
			c, f := newStateCore(t)
			ctx := context.Background()
			if err := SaveBotState(ctx, c.store, st); err != nil {
				t.Fatal(err)
			}

			c.Handle(ctx, adminEvent("/claim abc", "u1", "", host.SceneC2C, "u1"))
			c.Handle(ctx, gateEvent("hello", "u1", "g1"))

			if n := countRows(t, c.store, "messages"); n != 0 {
				t.Errorf("%s 不得落库，messages 实际 %d 行", st, n)
			}
			if n := countRows(t, c.store, "processed_events"); n != 0 {
				t.Errorf("%s 不得进幂等表，实际 %d 行", st, n)
			}
			if _, ok := f.lastBody(); ok {
				t.Errorf("%s 不得回任何消息", st)
			}
			if got := c.DroppedByState()[st]; got != 2 {
				t.Errorf("%s 的丢弃计数应为 2，实际 %d", st, got)
			}
		})
	}
}

// 认领成功 → active：闸门在认领那一条之后才放行普通事件。
func TestClaimMakesBotActive(t *testing.T) {
	probe := &probePlugin{manifest: host.Manifest{
		Name: "probe", APIVersion: host.APIVersion, Intents: []string{"message"},
	}}
	c, _ := newStateCore(t, probe)
	ctx := context.Background()
	if err := c.ensureOwner(ctx); err != nil {
		t.Fatalf("生成认领令牌: %v", err)
	}
	token := c.claimToken
	if token == "" {
		t.Fatal("ensureOwner 应生成令牌")
	}

	c.Handle(ctx, adminEvent("/claim "+token, "u1", "", host.SceneC2C, "u1"))

	if st := stateOf(t, c); st != BotStateActive {
		t.Fatalf("认领成功应迁移到 active，实际 %q", st)
	}
	// 状态必须落库，而不是只活在内存里：重启之后闸门要能读到同一个结论。
	saved, err := LoadBotState(ctx, c.store)
	if err != nil {
		t.Fatal(err)
	}
	if saved != BotStateActive {
		t.Fatalf("bot_state 里应是 active，实际 %q", saved)
	}
}

// 去重排在群准入之前：未放行群的消息会占一行 processed_events（重投不该
// 重复判定），但业务表零行、插件零命中。
func TestDedupeRunsBeforeGroupAdmission(t *testing.T) {
	c, _, probe, ctx := newGateCore(t)

	ev := gateEvent("hello", "u1", "g-not-allowed")
	c.Handle(ctx, ev)
	c.Handle(ctx, ev) // 平台重投

	if n := countRows(t, c.store, "processed_events"); n != 1 {
		t.Errorf("去重应排在群准入之前，processed_events 实际 %d 行", n)
	}
	if n := countRows(t, c.store, "messages"); n != 0 {
		t.Errorf("未放行群不得落库，messages 实际 %d 行", n)
	}
	if n := probe.hits.Load(); n != 0 {
		t.Errorf("未放行群不得触发插件，实际 %d 次", n)
	}
}

// 鉴权类业务码连续 3 次 → failing；握手成功（NoteConnected）→ active。
func TestThreeAuthFailuresEnterFailingThenReconnectActivates(t *testing.T) {
	c, _, _, ctx := newGateCore(t)
	ev := adminEvent("hi", "u1", "", host.SceneC2C, "u1")
	// 装饰器是生产路径上唯一把出站结果回灌状态机的地方，这里直接用它。
	c.sender.api = stateTrackingAPI{inner: authFailAPI{}, notify: c.noteSendResult}
	// 进 failing 必须顺手断一条连接，否则"链路还通着"时退避重连永远不发生。
	rc := &fakeReconnector{}
	c.SetReconnector(rc)

	for i := 1; i <= authFailureThreshold; i++ {
		if err := c.sender.Reply(ctx, ev, host.Reply{Content: "hi"}); err == nil {
			t.Fatal("鉴权类失败应返回错误")
		}
		if i < authFailureThreshold {
			if st := stateOf(t, c); st == BotStateFailing {
				t.Fatalf("第 %d 次就不该进 failing", i)
			}
		}
	}
	if st := stateOf(t, c); st != BotStateFailing {
		t.Fatalf("连续 %d 次鉴权失败应进 failing，实际 %q", authFailureThreshold, st)
	}
	if rc.calls != 1 {
		t.Fatalf("进 failing 应主动断开一次连接，实际 %d 次", rc.calls)
	}

	c.NoteConnected()
	if st := stateOf(t, c); st != BotStateActive {
		t.Fatalf("重连成功应回 active，实际 %q", st)
	}
}

// 网络类错误不是鉴权失败：连续 3 次也不该进 failing，否则一次网络抖动
// 就会让 Bot 停摆十几分钟。
func TestNetworkErrorsDoNotEnterFailing(t *testing.T) {
	c, _, _, ctx := newGateCore(t)
	ev := adminEvent("hi", "u1", "", host.SceneC2C, "u1")
	c.sender.api = stateTrackingAPI{inner: netFailAPI{}, notify: c.noteSendResult}

	for i := 0; i < authFailureThreshold+2; i++ {
		_ = c.sender.Reply(ctx, ev, host.Reply{Content: "hi"})
	}
	if st := stateOf(t, c); st != BotStateActive {
		t.Fatalf("网络错误不该进 failing，实际 %q", st)
	}
}

// ReconcileBotState 是启动时的对齐点：配置停用 → suspended，改回启用 →
// 按 admins 推导（空则 pending_claim）。
func TestReconcileBotState(t *testing.T) {
	db := newUsageStore(t)
	ctx := context.Background()

	st, err := ReconcileBotState(ctx, db, false)
	if err != nil {
		t.Fatal(err)
	}
	if st != BotStateSuspended {
		t.Fatalf("enabled=false 应写 suspended，实际 %q", st)
	}
	if saved, _ := LoadBotState(ctx, db); saved != BotStateSuspended {
		t.Fatalf("状态应落库，实际 %q", saved)
	}

	st, err = ReconcileBotState(ctx, db, true)
	if err != nil {
		t.Fatal(err)
	}
	if st != BotStatePendingClaim {
		t.Fatalf("解除停用时 admins 为空应回 pending_claim，实际 %q", st)
	}

	if err := db.UpsertAdmin(ctx, store.Admin{OpenID: "u1", Role: store.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := SaveBotState(ctx, db, BotStateSuspended); err != nil {
		t.Fatal(err)
	}
	if st, err = ReconcileBotState(ctx, db, true); err != nil {
		t.Fatal(err)
	}
	if st != BotStateActive {
		t.Fatalf("有管理员时解除停用应回 active，实际 %q", st)
	}
}

// 状态键只有一个：读写都用同一份常量，别处再拼字符串就会出现写读错位。
func TestBotStateKeyIsSingleSource(t *testing.T) {
	db := newUsageStore(t)
	ctx := context.Background()
	if err := SaveBotState(ctx, db, BotStateFailing); err != nil {
		t.Fatal(err)
	}
	v, ok, err := db.GetState(ctx, StateKeyBotState)
	if err != nil || !ok {
		t.Fatalf("应按 %q 读到状态: ok=%v err=%v", StateKeyBotState, ok, err)
	}
	if v != string(BotStateFailing) {
		t.Fatalf("bot_state 里应是 failing，实际 %q", v)
	}
}

// authFailAPI 让每次出站都以"密钥失效"失败。
type authFailAPI struct{}

func (authFailAPI) SendMessage(context.Context, string, string, api.MessageBody) error {
	return &api.BusinessError{Code: 100016, Message: "invalid appid or secret"}
}

func (authFailAPI) UploadMedia(context.Context, string, string, api.MediaUpload) (api.MediaUploadResult, error) {
	return api.MediaUploadResult{}, &api.BusinessError{Code: 100016, Message: "invalid appid or secret"}
}

// netFailAPI 模拟可恢复的传输层失败。
type netFailAPI struct{}

func (netFailAPI) SendMessage(context.Context, string, string, api.MessageBody) error {
	return errors.New("connection reset by peer")
}

func (netFailAPI) UploadMedia(context.Context, string, string, api.MediaUpload) (api.MediaUploadResult, error) {
	return api.MediaUploadResult{}, errors.New("connection reset by peer")
}

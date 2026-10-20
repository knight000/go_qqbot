package core

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

// newSessionBackend 用真库 + 真路由搭适配器：判定顺序依赖"路由表里有没有这个
// 插件名"，用 fake 会把最关键的悬挂分支测成自证。
func newSessionBackend(t *testing.T, db *store.DB, plugins ...host.Plugin) sessionBackend {
	t.Helper()
	router, err := NewRouter(plugins)
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	return sessionBackend{db: db, router: router, log: slog.New(slog.DiscardHandler)}
}

func sessionEvent(peer string) *host.Event {
	return &host.Event{
		ID: "e1", Kind: host.KindMessage, Scene: host.SceneGroup,
		PeerID: peer, SenderID: "u1", Content: "随便说点什么",
	}
}

// 同 peer 只允许一个持有者；持有者自己再 Await 是"改 state、重新计时"。
func TestAwaitBusyAndSelfRefresh(t *testing.T) {
	db := newUsageStore(t)
	a := newStub("md", "/md", host.PermUser, "")
	b := newStub("other", "/other", host.PermUser, "")
	backend := newSessionBackend(t, db, a, b)
	ctx := context.Background()
	ev := sessionEvent("g1")

	if err := backend.AwaitSession(ctx, "md", ev, []byte("first"), time.Minute); err != nil {
		t.Fatalf("首个持有者应登记成功: %v", err)
	}

	err := backend.AwaitSession(ctx, "other", ev, []byte("hijack"), time.Minute)
	if !errors.Is(err, host.ErrSessionBusy) {
		t.Fatalf("第二个插件应拿到 ErrSessionBusy，实际 %v", err)
	}
	row, _, _ := db.Session(ctx, "g1")
	if row.Plugin != "md" || string(row.State) != "first" {
		t.Fatalf("被拒的一方不得改动行: %+v", row)
	}

	created := row.CreatedAt
	if err := backend.AwaitSession(ctx, "md", ev, []byte("second"), 2*time.Minute); err != nil {
		t.Fatalf("同一插件再次 Await 应成功: %v", err)
	}
	row, _, _ = db.Session(ctx, "g1")
	if string(row.State) != "second" {
		t.Fatalf("state 应被覆盖，实际 %q", row.State)
	}
	if !row.CreatedAt.Equal(created) {
		t.Fatalf("自有会话刷新应保留 created_at: %s → %s", created, row.CreatedAt)
	}
	if !row.ExpiresAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("过期时间应被重算: %s", row.ExpiresAt)
	}
}

// 过期行按"不存在"处理：新插件不该被一条已经没人等的行挡住。
func TestAwaitReplacesExpiredRow(t *testing.T) {
	db := newUsageStore(t)
	backend := newSessionBackend(t, db, newStub("new", "/new", host.PermUser, ""))
	ctx := context.Background()

	past := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := db.ReplaceSession(ctx, store.Session{
		Peer: "g1", Scene: "group", Plugin: "old",
		State:     []byte("stale"),
		CreatedAt: past,
		ExpiresAt: past,
	}); err != nil {
		t.Fatal(err)
	}

	if err := backend.AwaitSession(ctx, "new", sessionEvent("g1"), []byte("fresh"), time.Minute); err != nil {
		t.Fatalf("过期行不该算冲突: %v", err)
	}
	row, _, _ := db.Session(ctx, "g1")
	if row.Plugin != "new" || string(row.State) != "fresh" {
		t.Fatalf("应换成新持有者: %+v", row)
	}
}

// 悬挂会话（持有者已卸载）同样不算冲突：否则新插件会被一个永远不会有人
// 消费的行挡住，用户只能等 5 分钟过期才能重来。
func TestAwaitReplacesOrphanRow(t *testing.T) {
	db := newUsageStore(t)
	backend := newSessionBackend(t, db, newStub("live", "/live", host.PermUser, ""))
	ctx := context.Background()

	if err := db.ReplaceSession(ctx, store.Session{
		Peer: "g1", Scene: "group", Plugin: "ghost",
		State:     []byte("orphan"),
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	if err := backend.AwaitSession(ctx, "live", sessionEvent("g1"), nil, time.Minute); err != nil {
		t.Fatalf("悬挂行不该算冲突: %v", err)
	}
	if row, _, _ := db.Session(ctx, "g1"); row.Plugin != "live" {
		t.Fatalf("应换成在加载集合里的持有者，实际 %q", row.Plugin)
	}
}

// --- Dispatcher 会话接管 ---

// fakeSessions 是 Dispatcher 的最小会话视图。用 fake 而不是真库，
// 是为了能构造"行还在但持有者已经不在"这类真实路径上很难摆出来的状态。
type fakeSessions struct {
	rows    map[string]sessionRow
	deleted []string
	err     error
}

func (f *fakeSessions) Session(_ context.Context, peer string) (sessionRow, bool, error) {
	if f.err != nil {
		return sessionRow{}, false, f.err
	}
	row, ok := f.rows[peer]
	return row, ok, nil
}

func (f *fakeSessions) DeleteSession(_ context.Context, peer string) error {
	f.deleted = append(f.deleted, peer)
	delete(f.rows, peer)
	return nil
}

// awaitingStub 会真的调 Host.Session().Await：会话行由第一轮自己写进去，
// 不是测试手工预置的——否则测的是"Dispatcher 能不能读一张我摆好的表"。
type awaitingStub struct {
	manifest host.Manifest
	session  host.Session

	mu      sync.Mutex
	states  []*host.SessionContext
	next    []byte
	err     error
	handleN int
}

func (p *awaitingStub) Manifest() host.Manifest { return p.manifest }

func (p *awaitingStub) Init(_ context.Context, h host.Host) error {
	p.session = h.Session()
	return nil
}

func (p *awaitingStub) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	p.mu.Lock()
	p.handleN++
	p.states = append(p.states, ev.Session)
	next := p.next
	err := p.err
	p.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if ev.Session == nil {
		// 第一轮：登记"下一条给我"，然后正常回复。
		if err := p.session.Await(ctx, ev, next, time.Minute); err != nil {
			return nil, err
		}
	}
	return &host.Reply{Content: p.manifest.Name + ":ok"}, nil
}

func (p *awaitingStub) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handleN
}

func (p *awaitingStub) lastSession() *host.SessionContext {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.states) == 0 {
		return nil
	}
	return p.states[len(p.states)-1]
}

// newSessionDispatcher 用真库 + 真 host.Service 装配，会话链路端到端。
func newSessionDispatcher(t *testing.T, db *store.DB, plugins ...host.Plugin) (*Dispatcher, *fakeAPI) {
	t.Helper()
	router, err := NewRouter(plugins)
	if err != nil {
		t.Fatalf("装配路由: %v", err)
	}
	log := slog.New(slog.DiscardHandler)
	adapter := sessionBackend{db: db, router: router, log: log}
	for _, p := range plugins {
		svc := host.NewService(host.Options{Name: p.Manifest().Name, Session: adapter})
		if err := p.Init(context.Background(), svc); err != nil {
			t.Fatalf("初始化插件 %s: %v", p.Manifest().Name, err)
		}
	}
	f := &fakeAPI{}
	d := NewDispatcher(router, NewSender(f, log, ActivePolicy{}, nil), log, time.Second)
	d.sessions = sessionRows{db}
	return d, f
}

func c2cEvent(content string) *host.Event {
	return &host.Event{
		ID: "c-" + content, Kind: host.KindMessage, Name: "C2C_MESSAGE_CREATE",
		Scene: host.SceneC2C, PeerID: "g1", SenderID: "u1", MsgID: "m-" + content,
		Content: content,
	}
}

// 两轮交互：第一轮登记 + 回复，第二轮的普通文本直接投给持有者，
// 静态路由上那个"订阅全部消息"的插件一次都不该被调用。
func TestSessionTwoRounds(t *testing.T) {
	db := newUsageStore(t)
	a := &awaitingStub{
		manifest: host.Manifest{
			Name: "md", APIVersion: host.APIVersion, Intents: []string{"message"},
			Commands: []host.Command{{Prefix: "/start", Priority: 10}},
		},
		next: []byte(`{"step":2}`),
	}
	// 订阅全部消息、无命令：没有会话接管的话，第二轮一定由它接。
	b := &awaitingStub{manifest: host.Manifest{
		Name: "all", APIVersion: host.APIVersion, Intents: []string{"message"},
	}}
	d, _ := newSessionDispatcher(t, db, a, b)
	ctx := context.Background()

	d.handle(ctx, groupEvent("/start", host.Principal{Role: host.RoleUser}))
	if a.calls() != 1 {
		t.Fatalf("第一轮应投给 md，实际 %d 次", a.calls())
	}
	if _, ok, _ := db.Session(ctx, "g1"); !ok {
		t.Fatal("第一轮之后应留下会话行")
	}

	d.handle(ctx, groupEvent("蓝色 32 开", host.Principal{Role: host.RoleUser}))
	if a.calls() != 2 {
		t.Fatalf("第二轮应投给持有者，实际 %d 次", a.calls())
	}
	if b.calls() != 0 {
		t.Fatalf("会话接管必须跳出静态匹配，实际 all 被调用 %d 次", b.calls())
	}
	sc := a.lastSession()
	if sc == nil {
		t.Fatal("第二轮应带 SessionContext")
	}
	if sc.Plugin != "md" || string(sc.State) != `{"step":2}` {
		t.Fatalf("SessionContext 内容不符: %+v", sc)
	}
	if _, ok, _ := db.Session(ctx, "g1"); ok {
		t.Fatal("消费后会话行应消失")
	}
}

// peer 是会话键：别的 peer 的消息照常走静态匹配。
func TestSessionDoesNotLeakAcrossPeers(t *testing.T) {
	db := newUsageStore(t)
	a := &awaitingStub{manifest: host.Manifest{
		Name: "md", APIVersion: host.APIVersion, Intents: []string{"message"},
		Commands: []host.Command{{Prefix: "/start", Priority: 10}},
	}}
	b := &awaitingStub{manifest: host.Manifest{
		Name: "all", APIVersion: host.APIVersion, Intents: []string{"message"},
	}}
	d, _ := newSessionDispatcher(t, db, a, b)
	ctx := context.Background()

	d.handle(ctx, groupEvent("/start", host.Principal{Role: host.RoleUser}))
	other := groupEvent("别处的话", host.Principal{Role: host.RoleUser})
	other.PeerID = "g2"
	other.ID = "e-other"
	d.handle(ctx, other)

	if b.calls() != 1 {
		t.Fatalf("另一个 peer 应走静态匹配，实际 all 被调用 %d 次", b.calls())
	}
	if _, ok, _ := db.Session(ctx, "g1"); !ok {
		t.Fatal("g1 的会话不该被 g2 的消息消费")
	}
}

// 消费即删：成功与失败都一样。一次失败的投递不该把会话状态留在库里
// 等第二条消息来消费（ADR-0004 决策 3）。
func TestSessionConsumedEvenOnPluginError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"成功", nil},
		{"失败", errors.New("插件炸了")},
	} {
		p := &awaitingStub{manifest: host.Manifest{
			Name: "md", APIVersion: host.APIVersion, Intents: []string{"message"},
		}, err: tc.err}
		d, _ := newSessionDispatcher(t, newUsageStore(t), p)
		fake := &fakeSessions{rows: map[string]sessionRow{
			"g1": {Plugin: "md", Scene: "group", State: []byte("s"), ExpiresAt: time.Now().Add(time.Minute)},
		}}
		d.sessions = fake

		d.handle(context.Background(), groupEvent("回答", host.Principal{Role: host.RoleUser}))
		if p.calls() != 1 {
			t.Fatalf("%s: 持有者应被调用一次，实际 %d", tc.name, p.calls())
		}
		if len(fake.rows) != 0 {
			t.Fatalf("%s: 消费后行应消失，实际 %+v", tc.name, fake.rows)
		}
	}
}

// 悬挂会话：持有者已卸载 → 删行、warn、落回静态路由，不静默吞掉消息。
func TestSessionOrphanFallsBackToStatic(t *testing.T) {
	p := &awaitingStub{manifest: host.Manifest{
		Name: "live", APIVersion: host.APIVersion, Intents: []string{"message"},
	}}
	d, _ := newSessionDispatcher(t, newUsageStore(t), p)
	fake := &fakeSessions{rows: map[string]sessionRow{
		"g1": {Plugin: "ghost", Scene: "group", State: []byte("x"), ExpiresAt: time.Now().Add(time.Minute)},
	}}
	d.sessions = fake

	d.handle(context.Background(), groupEvent("你好", host.Principal{Role: host.RoleUser}))
	if len(fake.rows) != 0 {
		t.Fatal("悬挂行应被删除")
	}
	if p.calls() != 1 {
		t.Fatalf("消息应落回静态路由，实际 %d 次", p.calls())
	}
	if p.lastSession() != nil {
		t.Fatal("静态投递不该带 SessionContext")
	}
}

// 权限不足不消费：既不删行（否则一次越权尝试就能顶掉管理员的会话），
// 也不能吞掉消息。
func TestSessionDeniedKeepsRowAndFallsBack(t *testing.T) {
	admin := &awaitingStub{manifest: host.Manifest{
		Name: "adminonly", APIVersion: host.APIVersion, Intents: []string{"message"},
		Permission: host.PermBotAdmin,
	}}
	pub := &awaitingStub{manifest: host.Manifest{
		Name: "pub", APIVersion: host.APIVersion, Intents: []string{"message"},
	}}
	d, _ := newSessionDispatcher(t, newUsageStore(t), admin, pub)
	fake := &fakeSessions{rows: map[string]sessionRow{
		"g1": {Plugin: "adminonly", Scene: "group", State: []byte("x"), ExpiresAt: time.Now().Add(time.Minute)},
	}}
	d.sessions = fake

	d.handle(context.Background(), groupEvent("我是普通成员", host.Principal{Role: host.RoleUser, GroupRole: host.GroupRoleMember}))
	if admin.calls() != 0 {
		t.Fatalf("无权限的持有者不该被调用，实际 %d 次", admin.calls())
	}
	if len(fake.rows) != 1 {
		t.Fatal("权限不足时不得删行")
	}
	if pub.calls() != 1 {
		t.Fatalf("消息应落回静态路由，实际 %d 次", pub.calls())
	}
}

// 过期等于行不存在：删行 + 落回静态路由（不需要额外的 expired 状态位）。
func TestSessionExpiredIsDropped(t *testing.T) {
	p := &awaitingStub{manifest: host.Manifest{
		Name: "md", APIVersion: host.APIVersion, Intents: []string{"message"},
	}}
	d, _ := newSessionDispatcher(t, newUsageStore(t), p)
	fake := &fakeSessions{rows: map[string]sessionRow{
		"g1": {Plugin: "md", Scene: "group", State: []byte("x"), ExpiresAt: time.Now().Add(-time.Second)},
	}}
	d.sessions = fake

	d.handle(context.Background(), groupEvent("迟到的回答", host.Principal{Role: host.RoleUser}))
	if len(fake.rows) != 0 {
		t.Fatal("过期行应被删除")
	}
	if p.calls() != 1 || p.lastSession() != nil {
		t.Fatalf("应走静态匹配，实际 %d 次 session=%v", p.calls(), p.lastSession())
	}
}

// scene 不一致不消费：群 openid 与用户 openid 取值万一相同，
// 单聊消息不得吃掉群会话。
func TestSessionSceneMismatchKeepsRow(t *testing.T) {
	p := &awaitingStub{manifest: host.Manifest{
		Name: "md", APIVersion: host.APIVersion, Intents: []string{"message"},
	}}
	d, _ := newSessionDispatcher(t, newUsageStore(t), p)
	fake := &fakeSessions{rows: map[string]sessionRow{
		"g1": {Plugin: "md", Scene: "group", State: []byte("x"), ExpiresAt: time.Now().Add(time.Minute)},
	}}
	d.sessions = fake

	d.handle(context.Background(), c2cEvent("单聊里说的话"))
	if len(fake.rows) != 1 {
		t.Fatal("scene 不匹配时不得删行")
	}
	if p.calls() != 1 || p.lastSession() != nil {
		t.Fatalf("应走静态匹配，实际 %d 次", p.calls())
	}
}

// 会话只推进"用户说的话"：通知事件一律不消费。
func TestSessionIgnoresNonMessage(t *testing.T) {
	p := &awaitingStub{manifest: host.Manifest{
		Name: "md", APIVersion: host.APIVersion, Intents: []string{"message"},
	}}
	d, _ := newSessionDispatcher(t, newUsageStore(t), p)
	fake := &fakeSessions{rows: map[string]sessionRow{
		"g1": {Plugin: "md", Scene: "group", State: []byte("x"), ExpiresAt: time.Now().Add(time.Minute)},
	}}
	d.sessions = fake

	ev := groupEvent("", host.Principal{})
	ev.Kind = host.KindNotice
	ev.Name = "GROUP_MSG_RECEIVE"
	d.handle(context.Background(), ev)

	if p.calls() != 0 {
		t.Fatalf("通知不该推进会话，实际 %d 次", p.calls())
	}
	if len(fake.rows) != 1 {
		t.Fatal("通知不该消费会话行")
	}
}

// concurrencyStub 数"同时在 Handle 里的协程数"，用来验证同 peer 串行。
type concurrencyStub struct {
	awaitingStub
	mu       sync.Mutex
	cur, max int
	done     chan struct{}
}

func (p *concurrencyStub) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	p.mu.Lock()
	p.cur++
	if p.cur > p.max {
		p.max = p.cur
	}
	p.mu.Unlock()

	time.Sleep(20 * time.Millisecond)

	p.mu.Lock()
	p.cur--
	p.mu.Unlock()

	reply, err := p.awaitingStub.Handle(ctx, ev)
	p.done <- struct{}{}
	return reply, err
}

func (p *concurrencyStub) maxConcurrency() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

// 同 peer 串行：会话推进与静态路由在同一个队列里排队，
// 插件对同一个 peer 不会同时看到两个事件（msg_seq 单调也依赖这条）。
func TestSessionSerializesWithStaticRouting(t *testing.T) {
	p := &concurrencyStub{
		awaitingStub: awaitingStub{manifest: host.Manifest{
			Name: "md", APIVersion: host.APIVersion, Intents: []string{"message"},
			Commands: []host.Command{{Prefix: "/start", Priority: 10}},
		}, next: []byte("s")},
		done: make(chan struct{}, 4),
	}
	db := newUsageStore(t)
	d, _ := newSessionDispatcher(t, db, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Dispatch(ctx, groupEvent("/start", host.Principal{Role: host.RoleUser}))
	second := groupEvent("下一条", host.Principal{Role: host.RoleUser})
	second.ID = "e-2"
	d.Dispatch(ctx, second)

	for i := 0; i < 2; i++ {
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			t.Fatal("两条消息没有都处理完")
		}
	}
	if got := p.maxConcurrency(); got != 1 {
		t.Fatalf("同 peer 的最大并发应为 1，实际 %d", got)
	}
}

func seedSession(t *testing.T, c *Core, peer, plugin string, expires time.Time) {
	t.Helper()
	if err := c.store.ReplaceSession(context.Background(), store.Session{
		Peer: peer, Scene: "group", Plugin: plugin,
		State:     []byte(`{"step":1}`),
		CreatedAt: time.Now(),
		ExpiresAt: expires,
	}); err != nil {
		t.Fatalf("预置会话 %s: %v", peer, err)
	}
}

func hasSession(t *testing.T, c *Core, peer string) bool {
	t.Helper()
	_, ok, err := c.store.Session(context.Background(), peer)
	if err != nil {
		t.Fatalf("读会话 %s: %v", peer, err)
	}
	return ok
}

// R1 / R2 / R3 都要带走会话：留下一个等不到输入的持有者，比直接不响应更难排查。
func TestResetLevelsClearSessions(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	ctx := context.Background()

	// R1：只清当前 peer。
	seedSession(t, c, "g1", "md", time.Now().Add(time.Minute))
	seedSession(t, c, "g2", "md", time.Now().Add(time.Minute))
	got := resetCall(t, c, f, ctx, "/reset session", "owner-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "接管 1 个") {
		t.Fatalf("R1 摘要应报出清掉的会话数，实际: %q", got)
	}
	if hasSession(t, c, "g1") {
		t.Fatal("R1 应清掉该 peer 的会话")
	}
	if !hasSession(t, c, "g2") {
		t.Fatal("R1 不该波及别的 peer")
	}

	// R2：预览 → 确认码 → 执行。
	seedSession(t, c, "g1", "md", time.Now().Add(time.Minute))
	got = resetCall(t, c, f, ctx, "/reset data", "owner-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "会话接管 1 个") {
		t.Fatalf("R2 预览应算上会话，实际: %q", got)
	}
	rec, ok, _ := c.store.PendingReset(ctx, "owner-1", "g1", store.ResetLevelData)
	if !ok {
		t.Fatal("应生成确认码")
	}
	got = resetCall(t, c, f, ctx, "/reset data "+rec.Code, "owner-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "会话接管 1 个") {
		t.Fatalf("R2 结果应报出会话数，实际: %q", got)
	}
	if hasSession(t, c, "g1") {
		t.Fatal("R2 应清掉该 peer 的会话")
	}

	// R3：全表。
	seedSession(t, c, "g2", "md", time.Now().Add(time.Minute))
	got = resetCall(t, c, f, ctx, "/reset all", "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "会话接管 1 个") {
		t.Fatalf("R3 预览应算上会话，实际: %q", got)
	}
	rec, ok, _ = c.store.PendingReset(ctx, "owner-1", store.ResetScopeGlobal, store.ResetLevelAll)
	if !ok {
		t.Fatal("R3 应生成确认码")
	}
	got = resetCall(t, c, f, ctx, "/reset all "+rec.Code+" 确认", "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "会话接管 1 个") {
		t.Fatalf("R3 结果应报出会话数，实际: %q", got)
	}
	if n, _ := c.store.CountSessions(ctx); n != 0 {
		t.Fatalf("R3 后会话表应为空，实际 %d", n)
	}
}

// GC 兜底清过期会话：再也没有人来读的那些行只靠它消失。
func TestGCPurgesExpiredSessions(t *testing.T) {
	c, _ := newAdminCore(t)
	c.cfg = &config.Config{}
	ctx := context.Background()

	seedSession(t, c, "expired", "md", time.Now().Add(-time.Minute))
	seedSession(t, c, "live", "md", time.Now().Add(time.Hour))

	c.runGC(ctx)

	if hasSession(t, c, "expired") {
		t.Fatal("过期会话应被 GC 清掉")
	}
	if !hasSession(t, c, "live") {
		t.Fatal("未过期的会话不得被清")
	}
}

// /group status 要能回答"这个群现在被谁占着、还剩多久"，
// 否则使用者只会看到"机器人为什么不理我"而查不到任何线索。
func TestGroupStatusShowsSession(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	ctx := context.Background()
	if err := c.store.SetGroupMode(ctx, "g1", store.GroupModeAllow); err != nil {
		t.Fatal(err)
	}

	got := resetCall(t, c, f, ctx, "/group status", "owner-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "会话 无") {
		t.Fatalf("无会话时应显示「会话 无」，实际: %q", got)
	}

	seedSession(t, c, "g1", "md", time.Now().Add(4*time.Minute+12*time.Second))
	got = resetCall(t, c, f, ctx, "/group status", "owner-1", host.SceneGroup, "g1")
	if !strings.Contains(got, "会话 插件 md") || !strings.Contains(got, "剩余") {
		t.Fatalf("应显示持有者与剩余时间，实际: %q", got)
	}
}

// 私聊总览给"有多少 peer 处于会话中"；0 时不输出这一项，与「已退出」同风格。
func TestGroupOverviewCountsSessions(t *testing.T) {
	c, f := newAdminCore(t)
	seedAdmin(t, c, "owner-1", "", store.RoleOwner)
	ctx := context.Background()

	got := resetCall(t, c, f, ctx, "/group status", "owner-1", host.SceneC2C, "owner-1")
	if strings.Contains(got, "会话中") {
		t.Fatalf("没有会话时不应输出这一项，实际: %q", got)
	}

	seedSession(t, c, "g1", "md", time.Now().Add(time.Minute))
	seedSession(t, c, "g2", "md", time.Now().Add(time.Minute))
	got = resetCall(t, c, f, ctx, "/group status", "owner-1", host.SceneC2C, "owner-1")
	if !strings.Contains(got, "会话中 2") {
		t.Fatalf("应显示进行中的会话数，实际: %q", got)
	}
}

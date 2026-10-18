package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

// fakeRunner 同时满足 host.Plugin 与 host.JobRunner：Bind 要求前者，
// 真正被调度的是后者。
type fakeRunner struct {
	mu    sync.Mutex
	peers []string
}

func (f *fakeRunner) Manifest() host.Manifest               { return host.Manifest{Name: "digest"} }
func (f *fakeRunner) Init(context.Context, host.Host) error { return nil }
func (f *fakeRunner) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}

func (f *fakeRunner) RunJob(_ context.Context, spec host.JobSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers = append(f.peers, spec.Peer)
	return nil
}

func (f *fakeRunner) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.peers...)
}

func newTestScheduler(t *testing.T) (*Scheduler, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	return NewScheduler(db, slog.New(slog.DiscardHandler)), db
}

func jobPeers(t *testing.T, db *store.DB, id string) map[string]bool {
	t.Helper()
	rows, err := db.QueryRows(context.Background(),
		`SELECT peer FROM jobs WHERE id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		peer, _ := r["peer"].(string)
		out[peer] = true
	}
	return out
}

// 全局任务不该因为库里有群就展开成 N 行——它本来就只跑一次。
func TestRegisterGlobalJobStaysSingleRow(t *testing.T) {
	s, db := newTestScheduler(t)
	ctx := context.Background()
	if err := db.MarkGroupPresent(ctx, "g1"); err != nil {
		t.Fatal(err)
	}

	if err := s.Register(host.JobSpec{Plugin: "digest", Name: "daily", Cron: "0 9 * * *"}); err != nil {
		t.Fatal(err)
	}
	peers := jobPeers(t, db, "digest:daily")
	if len(peers) != 1 || !peers[""] {
		t.Fatalf("全局任务应只有 peer='' 一行: %v", peers)
	}
}

// per-peer 任务在注册时就要覆盖所有在群，已退出的不建。
func TestRegisterPerPeerExpandsPresentGroups(t *testing.T) {
	s, db := newTestScheduler(t)
	ctx := context.Background()
	for _, g := range []string{"g1", "g2"} {
		if err := db.MarkGroupPresent(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MarkGroupLeft(ctx, "g2"); err != nil {
		t.Fatal(err)
	}

	if err := s.Register(host.JobSpec{
		Plugin: "digest", Name: "daily", Cron: "0 9 * * *", PerPeer: true,
	}); err != nil {
		t.Fatal(err)
	}

	peers := jobPeers(t, db, "digest:daily")
	if !peers["g1"] {
		t.Fatalf("在群的群应被展开: %v", peers)
	}
	if peers["g2"] {
		t.Fatalf("已退出的群不该建实例: %v", peers)
	}
	if peers[""] {
		t.Fatalf("per-peer 任务不该有全局行: %v", peers)
	}
}

// 补建必须是"只加不改"：把已跑过的 next_run_at 重算回下一个 cron 时刻，
// 等于群消息越密集、任务越永远轮不到。
func TestExpandPeerKeepsExistingNextRun(t *testing.T) {
	s, db := newTestScheduler(t)
	ctx := context.Background()
	if err := db.MarkGroupPresent(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(host.JobSpec{
		Plugin: "digest", Name: "daily", Cron: "0 9 * * *", PerPeer: true,
	}); err != nil {
		t.Fatal(err)
	}

	// 模拟"下次执行已被推进到很近的时刻"，再补一次 g1。
	if err := db.UpsertJob(ctx, store.Job{
		ID: "digest:daily", Peer: "g1", Plugin: "digest",
		Spec: "0 9 * * *", NextRunAt: 12345,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpandPeer(ctx, "g1"); err != nil {
		t.Fatal(err)
	}

	var next int64
	rows, err := db.QueryRows(ctx,
		`SELECT next_run_at FROM jobs WHERE id = 'digest:daily' AND peer = 'g1'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("g1 实例应存在: %v", rows)
	}
	next, _ = rows[0]["next_run_at"].(int64)
	if next != 12345 {
		t.Fatalf("补建覆盖了已有的 next_run_at: %d", next)
	}

	// 新群照样能补上。
	if err := db.MarkGroupPresent(ctx, "g3"); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpandPeer(ctx, "g3"); err != nil {
		t.Fatal(err)
	}
	if peers := jobPeers(t, db, "digest:daily"); !peers["g3"] {
		t.Fatalf("新群应被补建: %v", peers)
	}
}

// 执行时把目标群注入 spec：插件拿不到群列表，只可能从 Peer 知道该推给谁。
func TestRunInjectsPeerIntoJobSpec(t *testing.T) {
	s, db := newTestScheduler(t)
	ctx := context.Background()
	for _, g := range []string{"g1", "g2"} {
		if err := db.MarkGroupPresent(ctx, g); err != nil {
			t.Fatal(err)
		}
	}

	runner := &fakeRunner{}
	s.Bind("digest", runner)
	if err := s.Register(host.JobSpec{
		Plugin: "digest", Name: "daily", Cron: "0 9 * * *", PerPeer: true,
	}); err != nil {
		t.Fatal(err)
	}

	// 把两条都拉到"刚到期"，再逐个执行（run 是 runDue 里的单步）。
	// 不能设成 0：那会被误判为停机错过窗口而直接 skipped。
	now := time.Now()
	for _, peer := range []string{"g1", "g2"} {
		if err := db.UpsertJob(ctx, store.Job{
			ID: "digest:daily", Peer: peer, Plugin: "digest",
			Spec: "0 9 * * *", NextRunAt: now.Unix() - 10,
		}); err != nil {
			t.Fatal(err)
		}
	}

	jobs, err := db.DueJobs(ctx, now.Unix(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("两条都应到期，实际 %d", len(jobs))
	}
	for _, j := range jobs {
		s.run(ctx, j, now)
	}

	seen := runner.seen()
	if len(seen) != 2 {
		t.Fatalf("应执行两次，实际 %d", len(seen))
	}
	got := map[string]bool{seen[0]: true, seen[1]: true}
	if !got["g1"] || !got["g2"] || got[""] {
		t.Fatalf("Peer 注入错误: %v", seen)
	}

	// 执行历史也要带 peer，R2 才清得掉。
	runs, err := db.QueryRows(ctx,
		`SELECT peer FROM job_runs WHERE job_id = 'digest:daily'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("应记录两条执行历史，实际 %d", len(runs))
	}
	for _, r := range runs {
		if peer, _ := r["peer"].(string); peer == "" {
			t.Fatal("执行历史缺少 peer")
		}
	}
}

// 入群事件必须补建实例：重启时 Register 只遍历了当时的在群，
// 之后进来的群没有别的时机能被展开。
func TestBotJoinedGroupExpandsPerPeerJobs(t *testing.T) {
	c, _, ctx := newLifecycleCore(t)
	c.sched = NewScheduler(c.store, slog.New(slog.DiscardHandler))

	if err := c.sched.Register(host.JobSpec{
		Plugin: "digest", Name: "daily", Cron: "0 9 * * *", PerPeer: true,
	}); err != nil {
		t.Fatal(err)
	}
	c.Handle(ctx, lifecycleEvent(host.NoticeBotJoinedGroup, "g-new", "op-1"))

	peers := jobPeers(t, c.store, "digest:daily")
	if !peers["g-new"] {
		t.Fatalf("入群后应补建任务实例: %v", peers)
	}
}

// 首次见到某个群的消息时也要展开：退群事件丢失后重新入群、或平台在
// 机器人第一次说话前就把它拉进群，两种情况都只有消息能证明"它在群里"。
func TestFirstGroupMessageExpandsPerPeerJobs(t *testing.T) {
	c, _, ctx := newLifecycleCore(t)
	c.sched = NewScheduler(c.store, slog.New(slog.DiscardHandler))
	if err := c.sched.Register(host.JobSpec{
		Plugin: "digest", Name: "daily", Cron: "0 9 * * *", PerPeer: true,
	}); err != nil {
		t.Fatal(err)
	}

	// 群没进白名单，消息会被闸门丢弃——但登记与展开发生在判定之前，
	// 所以实例照样要建出来。
	c.Handle(ctx, adminEvent("hi", "u1", "", host.SceneGroup, "g-new"))

	peers := jobPeers(t, c.store, "digest:daily")
	if !peers["g-new"] {
		t.Fatalf("首次群消息后应补建任务实例: %v", peers)
	}
}

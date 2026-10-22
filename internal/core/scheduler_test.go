package core

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
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

func newTestScheduler(t testing.TB) (*Scheduler, *store.DB) {
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

// probeRunner 是可编排的 JobRunner：记账并发峰值、可阻塞、可制造失败，
// 用来验证"按插件分桶串行、桶间并行"的语义。
type probeRunner struct {
	plugin string
	global *globalProbe

	mu       sync.Mutex
	inflight int
	peak     int
	calls    []string

	hold     chan struct{} // 非 nil 时每个任务都等它关闭
	delay    time.Duration
	failPeer string
}

func (r *probeRunner) Manifest() host.Manifest               { return host.Manifest{Name: r.plugin} }
func (r *probeRunner) Init(context.Context, host.Host) error { return nil }
func (r *probeRunner) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}

func (r *probeRunner) RunJob(ctx context.Context, spec host.JobSpec) error {
	r.mu.Lock()
	r.inflight++
	if r.inflight > r.peak {
		r.peak = r.inflight
	}
	r.calls = append(r.calls, spec.Peer)
	r.mu.Unlock()

	if r.global != nil {
		r.global.enter()
	}
	defer func() {
		if r.global != nil {
			r.global.leave()
		}
		r.mu.Lock()
		r.inflight--
		r.mu.Unlock()
	}()

	if r.hold != nil {
		select {
		case <-r.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	if r.failPeer != "" && spec.Peer == r.failPeer {
		return errors.New("boom")
	}
	return nil
}

func (r *probeRunner) max() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak
}

func (r *probeRunner) ran() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// globalProbe 跨多个 runner 记账，用来验证并行度上限。
type globalProbe struct {
	mu       sync.Mutex
	inflight int
	peak     int
}

func (g *globalProbe) enter() {
	g.mu.Lock()
	g.inflight++
	if g.inflight > g.peak {
		g.peak = g.inflight
	}
	g.mu.Unlock()
}

func (g *globalProbe) leave() {
	g.mu.Lock()
	g.inflight--
	g.mu.Unlock()
}

func (g *globalProbe) max() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

func waitScheduler(t *testing.T, s *Scheduler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !s.Wait(ctx) {
		t.Fatal("调度任务未在 10s 内收尾")
	}
}

// registerPerPeer 让某插件按给定群展开 per-peer 任务，并让所有实例立即到期。
func registerPerPeer(t testing.TB, s *Scheduler, db *store.DB, plugin, name string, peers []string) {
	t.Helper()
	ctx := context.Background()
	for _, p := range peers {
		if err := db.MarkGroupPresent(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Register(host.JobSpec{
		Plugin: plugin, Name: name, Cron: "0 9 * * *", PerPeer: true,
	}); err != nil {
		t.Fatal(err)
	}
	dueNow(t, db, plugin+":"+name, plugin, peers)
}

func dueNow(t testing.TB, db *store.DB, id, plugin string, peers []string) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Unix() - 10
	if len(peers) == 0 {
		peers = []string{""}
	}
	for _, p := range peers {
		if err := db.UpsertJob(ctx, store.Job{
			ID: id, Peer: p, Plugin: plugin, Spec: "0 9 * * *", NextRunAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// 同一插件的实例必须串行——无论是同一个任务在多个群上展开，还是同一插件的
// 两个不同任务：插件实例按"调度器一次只调一个 Job"写的，并行只发生在插件之间。
func TestRunDueSerializesJobsOfSamePlugin(t *testing.T) {
	s, db := newTestScheduler(t)
	// 必须让任务真的持续一段时间：不延时的话几个实例可能"看起来串行"
	// 只是因为谁也来不及重叠，断言就验不出串行语义。
	r := &probeRunner{plugin: "digest", delay: 20 * time.Millisecond}
	s.Bind("digest", r)
	registerPerPeer(t, s, db, "digest", "tick", []string{"g1", "g2", "g3"})
	registerPerPeer(t, s, db, "digest", "sync", []string{"g4"})

	s.runDue()
	waitScheduler(t, s)

	if got := len(r.ran()); got != 4 {
		t.Fatalf("同一插件的四个实例都应执行，实际 %d", got)
	}
	if got := r.max(); got != 1 {
		t.Fatalf("同插件并发度应为 1，实际 %d", got)
	}
}

// 不同插件必须能同时在跑：一个 60 s 的慢任务不能把别的插件挡在门外。
func TestRunDueRunsDifferentPluginsInParallel(t *testing.T) {
	s, db := newTestScheduler(t)
	probe := &globalProbe{}
	slow := &probeRunner{plugin: "digest", global: probe, delay: 200 * time.Millisecond}
	fast := &probeRunner{plugin: "weather", global: probe, delay: 200 * time.Millisecond}
	s.Bind("digest", slow)
	s.Bind("weather", fast)
	registerPerPeer(t, s, db, "digest", "tick", []string{"g1"})
	registerPerPeer(t, s, db, "weather", "tick", []string{"g2"})

	s.runDue()
	waitScheduler(t, s)

	if len(slow.ran()) != 1 || len(fast.ran()) != 1 {
		t.Fatalf("两个插件的任务都应执行: %v / %v", slow.ran(), fast.ran())
	}
	if got := probe.max(); got != 2 {
		t.Fatalf("两个插件应同时执行（峰值并发 2），实际 %d", got)
	}
}

// 并行度上限：不论有多少插件到期，同时执行的不超过 jobParallel 个。
func TestRunDueCapsParallelism(t *testing.T) {
	s, db := newTestScheduler(t)
	probe := &globalProbe{}
	for i := 0; i < jobParallel*2; i++ {
		plugin := "p" + strconv.Itoa(i)
		peer := "g" + strconv.Itoa(i)
		s.Bind(plugin, &probeRunner{plugin: plugin, global: probe, delay: 40 * time.Millisecond})
		registerPerPeer(t, s, db, plugin, "tick", []string{peer})
	}

	s.runDue()
	waitScheduler(t, s)

	if got := probe.max(); got != jobParallel {
		t.Fatalf("并行度应为 %d，实际 %d", jobParallel, got)
	}
}

// 单轮扫描上限是 dueJobLimit：20 个群的实例不该需要两轮才轮完。
func TestRunDueDrainsOneTickUpToLimit(t *testing.T) {
	for _, n := range []int{20, 50, dueJobLimit} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			s, db := newTestScheduler(t)
			r := &probeRunner{plugin: "digest"}
			s.Bind("digest", r)
			peers := make([]string, 0, n)
			for i := 0; i < n; i++ {
				peers = append(peers, "g"+strconv.Itoa(i))
			}
			registerPerPeer(t, s, db, "digest", "tick", peers)

			s.runDue()
			waitScheduler(t, s)

			if got := len(r.ran()); got != n {
				t.Fatalf("digest:tick 的实例应在一轮内全部执行，实际 %d（单轮上限 %d？）", got, dueJobLimit)
			}
		})
	}
}

// 已被领走的插件，下一轮不能再领它新到期的实例。
//
// g2 在第一轮故意不到期：否则它会被同一个桶一起领走，这条用例就退化成
// "桶只领一次"，验不出 SQL 排除是否生效。
func TestRunDueSkipsBusyPlugin(t *testing.T) {
	s, db := newTestScheduler(t)
	ctx := context.Background()
	hold := make(chan struct{})
	slow := &probeRunner{plugin: "digest", hold: hold}
	fast := &probeRunner{plugin: "weather"}
	s.Bind("digest", slow)
	s.Bind("weather", fast)
	registerPerPeer(t, s, db, "digest", "tick", []string{"g1", "g2"})
	registerPerPeer(t, s, db, "weather", "tick", []string{"g3"})

	if err := db.UpsertJob(ctx, store.Job{
		ID: "digest:tick", Peer: "g2", Plugin: "digest",
		Spec: "0 9 * * *", NextRunAt: time.Now().Unix() + 3600,
	}); err != nil {
		t.Fatal(err)
	}

	s.runDue()
	deadline := time.Now().Add(5 * time.Second)
	for len(slow.ran()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("digest 未开始执行")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 慢任务横跨一个 tick：此时 g2 才到期，但 digest 还在忙。
	dueNow(t, db, "digest:tick", "digest", []string{"g2"})
	s.runDue()
	time.Sleep(50 * time.Millisecond) // 给"错误地又派一个桶"留出窗口

	if got := len(slow.ran()); got != 1 {
		t.Fatalf("digest 仍在执行时不该领新到期的实例，实际执行 %d 次", got)
	}

	close(hold)
	waitScheduler(t, s)
	// 这一轮结束后下一个 tick 才能领到 g2——它一直留在到期集合里，没被丢掉。
	s.runDue()
	waitScheduler(t, s)
	if got := len(slow.ran()); got != 2 {
		t.Fatalf("恢复后 g2 应被补上，实际执行 %d 次", got)
	}
}

// 慢任务不能延后其他插件的到期任务：快任务必须在慢任务还占着槽位时跑完。
func TestRunDueDoesNotDelayOtherPluginsBehindSlowJob(t *testing.T) {
	s, db := newTestScheduler(t)
	hold := make(chan struct{})
	slow := &probeRunner{plugin: "digest", hold: hold}
	fast := &probeRunner{plugin: "weather", delay: 5 * time.Millisecond}
	s.Bind("digest", slow)
	s.Bind("weather", fast)
	registerPerPeer(t, s, db, "digest", "tick", []string{"g1"})
	registerPerPeer(t, s, db, "weather", "tick", []string{"g2"})

	s.runDue()

	// fast 必须在 slow 仍被 hold 住的时候完成——这就是"没被延后"。
	deadline := time.Now().Add(5 * time.Second)
	for len(fast.ran()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("快任务被慢任务挡住了")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(slow.ran()) != 1 {
		t.Fatalf("慢任务应仍在执行，实际调用次数 %d", len(slow.ran()))
	}

	close(hold)
	waitScheduler(t, s)
}

// Stop 之后不再领新任务，并让在跑的任务立刻收到取消——/reload 路径下
// main 不会取消 Start 收到的 ctx，全靠这一下。
func TestStopCancelsRunningJobAndBlocksNewOnes(t *testing.T) {
	s, db := newTestScheduler(t)
	hold := make(chan struct{}) // 永不关闭：任务只能靠取消退出
	slow := &probeRunner{plugin: "digest", hold: hold}
	s.Bind("digest", slow)
	registerPerPeer(t, s, db, "digest", "tick", []string{"g1"})

	s.runDue()
	deadline := time.Now().Add(5 * time.Second)
	for len(slow.ran()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("任务未开始执行")
		}
		time.Sleep(5 * time.Millisecond)
	}

	s.Stop()
	// 在跑的任务因 ctx 取消而返回，不用等 hold。
	waitScheduler(t, s)

	// 停止之后即使有到期任务也不该再领。
	dueNow(t, db, "digest:tick", "digest", []string{"g1"})
	s.runDue()
	time.Sleep(50 * time.Millisecond)
	if got := len(slow.ran()); got != 1 {
		t.Fatalf("Stop 之后不该再执行任务，实际调用 %d 次", got)
	}
}

// 关停时正在跑的任务必须把收尾状态写进库。
//
// 收尾写入如果沿用 Stop 已经取消的 ctx，UpdateJob/RecordJobRun 会双双失败：
// next_run_at 停在原处、job_runs 少一行，5 分钟内重启时 DueJobs 仍认为它到期，
// 同一个任务在同一个群里再推一遍；超过 5 分钟又被判 skipped，静默不补。
func TestJobStatusPersistedAfterStop(t *testing.T) {
	s, db := newTestScheduler(t)
	hold := make(chan struct{}) // 永不关闭：任务只能靠取消退出
	defer close(hold)
	r := &probeRunner{plugin: "digest", hold: hold}
	s.Bind("digest", r)
	registerPerPeer(t, s, db, "digest", "tick", []string{"g1"})

	due := time.Now().Unix() - 10
	s.runDue()
	deadline := time.Now().Add(5 * time.Second)
	for len(r.ran()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("任务未开始执行")
		}
		time.Sleep(5 * time.Millisecond)
	}

	s.Stop()
	waitScheduler(t, s)

	ctx := context.Background()
	rows, err := db.QueryRows(ctx,
		`SELECT next_run_at, last_status FROM jobs WHERE id = 'digest:tick' AND peer = 'g1'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有一行任务状态，实际 %d", len(rows))
	}
	next, _ := rows[0]["next_run_at"].(int64)
	if next <= due {
		t.Fatalf("关停后 next_run_at 应已推进（原值 %d，实际 %d）", due, next)
	}
	if status, _ := rows[0]["last_status"].(string); status != "error" {
		t.Fatalf("被取消的任务应记 error，实际 %q", status)
	}

	runs, err := db.QueryRows(ctx, `SELECT status FROM job_runs WHERE job_id = 'digest:tick'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("job_runs 应有一行，实际 %d（收尾写入被关停打断了）", len(runs))
	}
}

// Wait 的 false 分支：任务还在跑、期限先到。
func TestWaitReportsTimeoutWhileJobRunning(t *testing.T) {
	s, db := newTestScheduler(t)
	hold := make(chan struct{})
	slow := &probeRunner{plugin: "digest", hold: hold}
	s.Bind("digest", slow)
	registerPerPeer(t, s, db, "digest", "tick", []string{"g1"})

	s.runDue()
	deadline := time.Now().Add(5 * time.Second)
	for len(slow.ran()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("任务未开始执行")
		}
		time.Sleep(5 * time.Millisecond)
	}

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if s.Wait(short) {
		t.Fatal("任务仍在跑时 Wait 应报告超时")
	}

	close(hold)
	waitScheduler(t, s)
}

// 熔断按 (job, peer) 计：并行化不能把两个实例的失败计数搅在一起。
func TestRunDueKeepsFailuresPerJobRow(t *testing.T) {
	s, db := newTestScheduler(t)
	r := &probeRunner{plugin: "digest", failPeer: "g1"}
	s.Bind("digest", r)
	registerPerPeer(t, s, db, "digest", "tick", []string{"g1", "g2"})

	s.runDue()
	waitScheduler(t, s)

	rows, err := db.QueryRows(context.Background(),
		`SELECT peer, consecutive_failures, enabled FROM jobs WHERE id = 'digest:tick'`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, row := range rows {
		peer, _ := row["peer"].(string)
		failures, _ := row["consecutive_failures"].(int64)
		got[peer] = failures
		if enabled, _ := row["enabled"].(int64); enabled != 1 {
			t.Fatalf("%s 只失败一次不该被停用", peer)
		}
	}
	if got["g1"] != 1 {
		t.Fatalf("g1 应记一次失败，实际 %d", got["g1"])
	}
	if got["g2"] != 0 {
		t.Fatalf("g2 成功不该被记失败，实际 %d", got["g2"])
	}
}

// BenchmarkRunDue 量一轮扫描的端到端耗时（含取任务、执行、写回 jobs/job_runs）。
//
// 两组对照：
//   - parallel/N：N 个插件各一行，走分桶并行（并行度 4）
//   - single/N  ：一个插件 N 行，桶内串行——这是旧实现里"20 个群"的真实形态
//
// 每个任务模拟 20 ms 的 I/O 工作，量级接近一次 HTTP + 一次写库。
func BenchmarkRunDue(b *testing.B) {
	for _, n := range []int{20, 50, 100} {
		b.Run("parallel/"+strconv.Itoa(n), func(b *testing.B) {
			benchRunDue(b, n, 1, 20*time.Millisecond)
		})
		b.Run("single/"+strconv.Itoa(n), func(b *testing.B) {
			benchRunDue(b, 1, n, 20*time.Millisecond)
		})
	}
}

// benchRunDue 建 plugins 个插件、每个 jobsPerPlugin 个实例。plugins=1 时
// 退化成"一个插件 N 个群"的单桶串行，也就是旧实现里的真实形态。
func benchRunDue(b *testing.B, plugins, jobsPerPlugin int, work time.Duration) {
	b.Helper()
	s, db := newTestScheduler(b)
	ctx := context.Background()

	type row struct{ id, peer, plugin string }
	rows := make([]row, 0, plugins*jobsPerPlugin)
	for i := 0; i < plugins; i++ {
		plugin := "p" + strconv.Itoa(i)
		s.Bind(plugin, &probeRunner{plugin: plugin, delay: work})
		peers := make([]string, 0, jobsPerPlugin)
		for k := 0; k < jobsPerPlugin; k++ {
			peers = append(peers, "g"+strconv.Itoa(i)+"-"+strconv.Itoa(k))
		}
		registerPerPeer(b, s, db, plugin, "tick", peers)
		for _, p := range peers {
			rows = append(rows, row{plugin + ":tick", p, plugin})
		}
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 把上一轮推进过的 next_run_at 重新拉回"已到期"，不计量这部分。
		b.StopTimer()
		for _, r := range rows {
			if err := db.UpsertJob(ctx, store.Job{
				ID: r.id, Peer: r.peer, Plugin: r.plugin,
				Spec: "0 9 * * *", NextRunAt: time.Now().Unix() - 10,
			}); err != nil {
				b.Fatal(err)
			}
		}
		b.StartTimer()

		s.runDue()
		if !s.Wait(runCtx) {
			b.Fatal("任务未在期限内收尾")
		}
	}
}

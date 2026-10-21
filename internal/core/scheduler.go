package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/robfig/cron/v3"
)

// Scheduler 采用「库表驱动 + 30 s 扫描」而不是每任务一个 timer：
// 设备休眠/时钟跳变时 timer 会漂移，扫描式每轮按绝对时间重算，天然自愈（§5.1）。
//
// 执行模型是「按插件分桶串行、桶间并行」：同一插件的所有到期实例排成一个桶、
// 桶内顺序执行，桶之间最多 jobParallel 个同时跑。分桶的粒度是插件而不是
// (插件, 任务名)，因为插件实例是按「调度器一次只调一个 Job」写的——并发只
// 允许发生在插件之间。
type Scheduler struct {
	db     *store.DB
	log    *slog.Logger
	tz     *time.Location
	parser cron.Parser

	// slots 是并行度信号量，桶启动前占用、结束后释放；容量即并行度。
	slots chan struct{}
	// idle 在 running 从 0 变 1 时重建、归零时关闭，供 Wait 使用。
	idle    chan struct{}
	running int

	mu      sync.Mutex
	specs   map[string]host.JobSpec
	runners map[string]host.JobRunner
	busy    map[string]bool // plugin -> 该插件的桶已被领走（含还在等槽的）
	started bool
	stopped bool
	// runCtx 是调度器自己的生命周期 ctx：在跑的任务与扫描都用它，Stop 取消它。
	// 不能只依赖 Start 收到的 ctx：/reload 走 RestartRequests 分支，main 在
	// Close 之前都没有取消那个 ctx。
	runCtx  context.Context
	stopRun context.CancelFunc
}

const (
	tickInterval = 30 * time.Second
	missGrace    = 300 * time.Second // 停机超过 5 分钟不补跑
	maxBackoff   = 16 * time.Minute

	// dueJobLimit 是单轮扫描上限。旧值 20 在 per-peer 任务铺开到 20 个群时
	// 刚好一轮扫不完（任务 3 s × 20 = 60 s > 30 s tick），下一轮必然迟到。
	// 100 覆盖当前规模的 5 倍余量，同时不至于一次把内存里的桶撑爆。
	dueJobLimit = 100

	// writeBackTimeout 是"任务状态回写"的期限。它必须短：关停时这段写入
	// 挡在关库之前，写不动就该让上层以非 0 退出，而不是无限等。
	writeBackTimeout = 2 * time.Second

	// jobParallel 是同时执行的桶数。调度任务是 I/O 型的（HTTP + SQLite 写），
	// 低配机上真正的瓶颈是出站配额与单写连接而不是 CPU；4 与 dispatch 的
	// 16 令牌、render 的 2 并发形成梯度，既能让慢任务不再挡住别人，又不会
	// 把平台配额在瞬时打满。暂不做成配置项：没有实测依据的旋钮只会让
	// 运维多一个可调错的地方。
	jobParallel = 4
)

func NewScheduler(db *store.DB, log *slog.Logger) *Scheduler {
	tz, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		log.Warn("加载时区失败，回退 UTC", "err", err)
		tz = time.UTC
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	return &Scheduler{
		db:      db,
		log:     log,
		tz:      tz,
		parser:  cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
		slots:   make(chan struct{}, jobParallel),
		runCtx:  runCtx,
		stopRun: stopRun,
		specs:   make(map[string]host.JobSpec),
		runners: make(map[string]host.JobRunner),
		busy:    make(map[string]bool),
	}
}

// Location 是"一天/一个 cron 时刻"的唯一时区来源。
//
// 主动消息日预算的切日必须与 cron 用同一个 location（见 NewSender）：
// 两者分叉时，一个平台自然日内可以花掉两个桶的额度，最多 2×limit。
func (s *Scheduler) Location() *time.Location { return s.tz }

// Bind 把插件实例登记为任务执行者（Core 在 Init 之前调用）。
func (s *Scheduler) Bind(plugin string, p host.Plugin) {
	if r, ok := p.(host.JobRunner); ok {
		s.mu.Lock()
		s.runners[plugin] = r
		s.mu.Unlock()
	}
}

// Register 仅启动期可用；之后返回错误，避免运行中动态改表。
//
// per-peer 任务在注册时就要给"当前所有在群"各建一行：之后新进来的群
// 由 ExpandPeer 补，退群的群由 DueJobs 的 left_at 过滤自动停掉。
func (s *Scheduler) Register(spec host.JobSpec) error {
	if spec.Plugin == "" || spec.Name == "" {
		return fmt.Errorf("任务必须有 plugin 与 name")
	}
	if spec.Cron == "" {
		return fmt.Errorf("任务 %s 缺少 cron 表达式", spec.Name)
	}
	sched, err := s.parser.Parse(spec.Cron)
	if err != nil {
		return fmt.Errorf("任务 %s 的 cron 非法: %w", spec.Name, err)
	}
	if spec.Timeout <= 0 {
		spec.Timeout = 60 * time.Second
	}
	spec.Peer = ""

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("任务 %s 注册过晚：仅允许启动期注册", spec.Name)
	}
	s.specs[spec.ID()] = spec
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	next := sched.Next(time.Now().In(s.tz)).Unix()
	if !spec.PerPeer {
		return s.upsert(ctx, spec, "", next)
	}
	// 一个群都没有不是错误：任务照样注册在内存里，新群入群时会补行。
	groups, err := s.db.ListGroupsInUse(ctx)
	if err != nil {
		return fmt.Errorf("展开任务 %s 的群实例: %w", spec.Name, err)
	}
	for _, g := range groups {
		if err := s.upsert(ctx, spec, g.OpenID, next); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) upsert(ctx context.Context, spec host.JobSpec, peer string, next int64) error {
	return s.db.UpsertJob(ctx, store.Job{
		ID:        spec.ID(),
		Peer:      peer,
		Plugin:    spec.Plugin,
		Spec:      spec.Cron,
		Timezone:  s.tz.String(),
		NextRunAt: next,
		Payload:   spec.Payload,
	})
}

// ExpandPeer 给某个群补齐所有 per-peer 任务的实例行，幂等。
//
// 调用时机：机器人入群、以及第一次见到某个群的消息（事件丢失时的自愈）。
// 用的是"只插缺失行"，不会把已跑过任务的 next_run_at 推回下一个 cron 时刻。
func (s *Scheduler) ExpandPeer(ctx context.Context, peer string) error {
	if peer == "" {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	specs := make([]host.JobSpec, 0, 4)
	for _, sp := range s.specs {
		if sp.PerPeer {
			specs = append(specs, sp)
		}
	}
	s.mu.Unlock()
	if len(specs) == 0 {
		return nil
	}

	for _, sp := range specs {
		// 与 Register 用同一个"下一次"算法，保证新入群的群与本批重启后的
		// 其他群落在同一批时刻上，不会有某个群永远偏移半分钟。
		next := s.next(sp.Cron, now)
		if _, err := s.db.InsertJobIfMissing(ctx, store.Job{
			ID:        sp.ID(),
			Peer:      peer,
			Plugin:    sp.Plugin,
			Spec:      sp.Cron,
			Timezone:  s.tz.String(),
			NextRunAt: next,
			Payload:   sp.Payload,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	s.started = true
	keep := make([]string, 0, len(s.specs))
	perPeer := 0
	for id, sp := range s.specs {
		keep = append(keep, id)
		if sp.PerPeer {
			perPeer++
		}
	}
	s.mu.Unlock()

	// 插件被删除后，其任务行保留但停用（保留执行历史）
	if err := s.db.DisableJobsExcept(ctx, keep); err != nil {
		return err
	}
	// per-peer 数量必须单独打出来：只有一个群都没进时它是 0，
	// 而"任务已注册"与"任务正跑在 0 个群上"在日志里长得一模一样。
	s.log.Info("调度器启动", "任务", len(keep), "per_peer", perPeer)

	// 父 ctx 取消（信号退出）等价于 Stop；/reload 路径父 ctx 不会取消，
	// 由 Core.Shutdown 显式调用 Stop。
	go func() {
		select {
		case <-ctx.Done():
			s.Stop()
		case <-s.runCtx.Done():
		}
	}()
	go s.loop()
	return nil
}

// Stop 停止调度：此后的扫描不再领任务，在跑的任务收到取消信号。
//
// 必须在 Wait 之前调用。只依赖 Start 收到的 ctx 是不够的：/reload 走
// RestartRequests 分支，main 到 Close 之前都没有取消那个 ctx，此时不主动
// 停就会在关插件、关库之后继续起新桶。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	cancel := s.stopRun
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Scheduler) loop() {
	t := time.NewTicker(tickInterval)
	defer t.Stop()

	s.runDue()
	for {
		select {
		case <-s.runCtx.Done():
			return
		case <-t.C:
			s.runDue()
		}
	}
}

func (s *Scheduler) runDue() {
	if s.isStopped() {
		return
	}
	now := time.Now()
	// 忙的插件必须在 SQL 里排除，不能取回来再在内存里丢：limit 是按
	// next_run_at 取的，一个卡住的插件积压上百行 per-peer 实例时会占满
	// 整个窗口，其他插件的到期任务永远轮不到。
	jobs, err := s.db.DueJobs(s.runCtx, now.Unix(), dueJobLimit, s.busyPlugins()...)
	if err != nil {
		s.log.Error("扫描任务失败", "err", err)
		return
	}
	for _, bucket := range groupByPlugin(jobs) {
		s.launch(bucket, now)
	}
}

func (s *Scheduler) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// busyPlugins 取当前正在执行的插件快照。调用方在 runDue 里只用于 SQL 排除，
// 真正的"同一插件只有一个桶"由 launch 里的忙判重保证——两次快照之间可能有
// 别的 tick 启动新桶，但那只会让我们多查几行，不会让同一插件跑两个桶。
func (s *Scheduler) busyPlugins() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.busy) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.busy))
	for p := range s.busy {
		out = append(out, p)
	}
	return out
}

// jobBucket 是一轮扫描里属于同一个插件的到期实例。
type jobBucket []store.Job

// groupByPlugin 把一批到期任务按插件分桶，桶的顺序按各插件首次到期的时间。
//
// jobs 已由 DueJobs 按 next_run_at 升序排好，按首次出现分组即可保持这个顺序；
// 用 map 只做"桶归并"，不遍历 map 取值，避免顺序随机。
func groupByPlugin(jobs []store.Job) []jobBucket {
	if len(jobs) == 0 {
		return nil
	}
	index := make(map[string]int, len(jobs))
	buckets := make([]jobBucket, 0, len(jobs))
	for _, j := range jobs {
		i, ok := index[j.Plugin]
		if !ok {
			i = len(buckets)
			index[j.Plugin] = i
			buckets = append(buckets, nil)
		}
		buckets[i] = append(buckets[i], j)
	}
	return buckets
}

// launch 把一个桶交给协程执行，桶内顺序执行、桶间受 slots 限流。
//
// 异步而不是在 runDue 里等，是为了让"下一轮扫描"不被慢任务推迟：一个 60 s
// 的任务如果同步阻塞 30 s 的 tick，其他插件在这一分钟内到期的任务全部要等到
// 下一轮。异步之后，下一轮只会因为该插件还在忙而跳过它自己的行。
//
// 代价是没有行锁，靠进程内的 busy 表防止同一个插件被两轮同时领走；这也意味着
// 多实例部署时保护失效（当前由单实例锁保证只有一个进程，见 §2.1）。
func (s *Scheduler) launch(jobs jobBucket, now time.Time) {
	if len(jobs) == 0 {
		return
	}
	ctx := s.runCtx
	// 已取消就不再起新桶：Stop 之后起的新桶会让任务在 db.Close 之后才跑起来。
	if ctx.Err() != nil {
		return
	}
	plugin := jobs[0].Plugin

	s.mu.Lock()
	// stopped 与 busy 一起判：Wait 的快路径（running==0）与这里必须是同一个
	// 临界区，否则 Stop 之后仍可能有桶挤进来，Wait 提前返回。
	if s.stopped || s.busy[plugin] {
		s.mu.Unlock()
		return
	}
	s.busy[plugin] = true
	if s.running == 0 {
		s.idle = make(chan struct{})
	}
	s.running++
	s.mu.Unlock()

	go func() {
		defer s.finish(plugin)

		// 拿不到槽位就等着：桶已经被标记为忙，这一轮不执行的话下一轮也不会
		// 重复领它，等槽位比丢弃更划算。
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		case <-ctx.Done():
			return
		}

		for _, j := range jobs {
			if ctx.Err() != nil {
				return
			}
			s.run(ctx, j, now)
		}
	}()
}

// finish 清掉忙标记并维护 Wait 的收尾信号。
func (s *Scheduler) finish(plugin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, plugin)
	s.running--
	if s.running == 0 {
		close(s.idle)
	}
}

// Wait 等在跑的任务收尾，返回 false 表示 ctx 先到期。
//
// 关停顺序是 Stop → Wait → 关插件 → 关库。Stop 让在跑的任务收到取消（它们
// 大多在等 HTTP 或写库，会立刻返回），Wait 只负责等它们真正退出。晚于
// db.Close 的写入只会得到一串 "database is closed"，而在插件 Close 之后
// 回调它同样是错的。
func (s *Scheduler) Wait(ctx context.Context) bool {
	for {
		s.mu.Lock()
		if s.running == 0 {
			s.mu.Unlock()
			return true
		}
		idle := s.idle
		s.mu.Unlock()

		// 等到这一批清空之后要重新检查：清空的瞬间可能有新桶进来
		// （测试与 /reload 的关停路径下 ctx 未必先取消），一次 select 不够。
		select {
		case <-idle:
		case <-ctx.Done():
			return false
		}
	}
}

func (s *Scheduler) run(ctx context.Context, j store.Job, now time.Time) {
	spec, ok := s.spec(j.ID)
	if !ok {
		s.log.Warn("任务已无插件注册，停用", "job", j.ID, "peer", j.Peer)
		j.Enabled = false
		s.saveJob(ctx, j)
		return
	}
	// per-peer 实例的目标群在注册时并不存在，只能由核心在执行前注入。
	spec.Peer = j.Peer

	// 停机错过：标记 skipped 并推进，不补跑（避免重启雪崩）
	if now.Sub(time.Unix(j.NextRunAt, 0)) > missGrace {
		next := s.next(j.Spec, now)
		j.LastRunAt = now.Unix()
		j.LastStatus = "skipped"
		j.NextRunAt = next
		wctx, wcancel := writeBackCtx(ctx)
		_ = s.db.UpdateJob(wctx, j)
		_ = s.db.RecordJobRun(wctx, j.ID, j.Peer, now.Unix(), now.Unix(), "skipped", "错过执行窗口")
		wcancel()
		s.log.Warn("任务错过执行窗口，跳过", "job", j.ID, "peer", j.Peer)
		return
	}

	runner := s.runner(j.Plugin)
	if runner == nil {
		s.log.Error("插件未实现 JobRunner，停用任务", "job", j.ID, "peer", j.Peer)
		j.Enabled = false
		s.saveJob(ctx, j)
		return
	}

	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, s.timeout(spec))
	err := s.call(runCtx, runner, spec)
	cancel()
	finished := time.Now()

	status := "ok"
	var errMsg string
	failures := 0
	if err != nil {
		errMsg = err.Error()
		status = "error"
		if errors.Is(err, context.DeadlineExceeded) {
			status = "timeout"
		}
		failures = j.ConsecutiveFailures + 1
	}

	next := s.next(j.Spec, finished)
	if err != nil {
		backoff := time.Minute << uint(minInt(failures-1, 4))
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		if at := finished.Add(backoff); at.After(time.Unix(next, 0)) {
			next = at.Unix()
		}
	}
	enabled := failures < 5

	j.LastRunAt = finished.Unix()
	j.LastStatus = status
	j.NextRunAt = next
	j.ConsecutiveFailures = failures
	j.Enabled = enabled
	// 收尾写入不继承取消：任务已经被 Stop 打断了，但"它跑过、下次什么时候跑"
	// 必须落盘，否则重启后同一个任务会再跑一遍（P2-2）。
	wctx, wcancel := writeBackCtx(ctx)
	defer wcancel()
	if err := s.db.UpdateJob(wctx, j); err != nil {
		s.log.Error("更新任务状态失败", "job", j.ID, "err", err)
	}
	_ = s.db.RecordJobRun(wctx, j.ID, j.Peer, started.Unix(), finished.Unix(), status, errMsg)

	if err != nil {
		s.log.Error("任务执行失败", "job", j.ID, "peer", j.Peer, "status", status, "failures", failures, "err", err)
		if !enabled {
			s.log.Error("任务连续失败 5 次，已自动停用", "job", j.ID, "peer", j.Peer)
		}
		return
	}
	s.log.Info("任务执行成功", "job", j.ID, "peer", j.Peer, "cost", finished.Sub(started).String())
}

// writeBackCtx 是"任务状态回写"专用的 ctx：不继承取消信号，只受短超时约束。
//
// 任务执行本身仍然跟着 runCtx 走（Stop 一到就取消），只有回写这一步例外。
// 关停时这两件事必须分开：不取消执行就等不到收尾，取消回写就会丢掉
// next_run_at 与 job_runs（P2-2）。
func writeBackCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), writeBackTimeout)
}

// saveJob 用不继承取消的 ctx 回写单行任务状态，失败只记日志：
// 这里的写入都是"顺手维护"，任务本身已经有结论了。
func (s *Scheduler) saveJob(ctx context.Context, j store.Job) {
	wctx, cancel := writeBackCtx(ctx)
	defer cancel()
	if err := s.db.UpdateJob(wctx, j); err != nil {
		s.log.Error("更新任务状态失败", "job", j.ID, "peer", j.Peer, "err", err)
	}
}

func (s *Scheduler) call(ctx context.Context, runner host.JobRunner, spec host.JobSpec) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("job panic: %v", rec)
		}
	}()
	return runner.RunJob(ctx, spec)
}

func (s *Scheduler) spec(id string) (host.JobSpec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.specs[id]
	return sp, ok
}

func (s *Scheduler) runner(plugin string) host.JobRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runners[plugin]
}

func (s *Scheduler) timeout(spec host.JobSpec) time.Duration {
	if spec.Timeout <= 0 {
		return 60 * time.Second
	}
	return spec.Timeout
}

func (s *Scheduler) next(expr string, from time.Time) int64 {
	sched, err := s.parser.Parse(expr)
	if err != nil {
		return from.Add(time.Hour).Unix()
	}
	return sched.Next(from.In(s.tz)).Unix()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

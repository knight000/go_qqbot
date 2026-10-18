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
type Scheduler struct {
	db     *store.DB
	log    *slog.Logger
	tz     *time.Location
	parser cron.Parser

	mu      sync.Mutex
	specs   map[string]host.JobSpec
	runners map[string]host.JobRunner
	started bool
}

const (
	tickInterval = 30 * time.Second
	missGrace    = 300 * time.Second // 停机超过 5 分钟不补跑
	maxBackoff   = 16 * time.Minute
)

func NewScheduler(db *store.DB, log *slog.Logger) *Scheduler {
	tz, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		log.Warn("加载时区失败，回退 UTC", "err", err)
		tz = time.UTC
	}
	return &Scheduler{
		db:      db,
		log:     log,
		tz:      tz,
		parser:  cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
		specs:   make(map[string]host.JobSpec),
		runners: make(map[string]host.JobRunner),
	}
}

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
	go s.loop(ctx)
	return nil
}

func (s *Scheduler) loop(ctx context.Context) {
	t := time.NewTicker(tickInterval)
	defer t.Stop()

	s.runDue(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runDue(ctx)
		}
	}
}

func (s *Scheduler) runDue(ctx context.Context) {
	now := time.Now()
	jobs, err := s.db.DueJobs(ctx, now.Unix(), 20)
	if err != nil {
		s.log.Error("扫描任务失败", "err", err)
		return
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		s.run(ctx, j, now)
	}
}

func (s *Scheduler) run(ctx context.Context, j store.Job, now time.Time) {
	spec, ok := s.spec(j.ID)
	if !ok {
		s.log.Warn("任务已无插件注册，停用", "job", j.ID, "peer", j.Peer)
		j.Enabled = false
		_ = s.db.UpdateJob(ctx, j)
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
		_ = s.db.UpdateJob(ctx, j)
		_ = s.db.RecordJobRun(ctx, j.ID, j.Peer, now.Unix(), now.Unix(), "skipped", "错过执行窗口")
		s.log.Warn("任务错过执行窗口，跳过", "job", j.ID, "peer", j.Peer)
		return
	}

	runner := s.runner(j.Plugin)
	if runner == nil {
		s.log.Error("插件未实现 JobRunner，停用任务", "job", j.ID, "peer", j.Peer)
		j.Enabled = false
		_ = s.db.UpdateJob(ctx, j)
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
	if err := s.db.UpdateJob(ctx, j); err != nil {
		s.log.Error("更新任务状态失败", "job", j.ID, "err", err)
	}
	_ = s.db.RecordJobRun(ctx, j.ID, j.Peer, started.Unix(), finished.Unix(), status, errMsg)

	if err != nil {
		s.log.Error("任务执行失败", "job", j.ID, "peer", j.Peer, "status", status, "failures", failures, "err", err)
		if !enabled {
			s.log.Error("任务连续失败 5 次，已自动停用", "job", j.ID, "peer", j.Peer)
		}
		return
	}
	s.log.Info("任务执行成功", "job", j.ID, "peer", j.Peer, "cost", finished.Sub(started).String())
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

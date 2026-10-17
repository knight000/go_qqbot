package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/store"
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

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("任务 %s 注册过晚：仅允许启动期注册", spec.Name)
	}
	s.specs[spec.ID()] = spec
	s.mu.Unlock()

	next := sched.Next(time.Now().In(s.tz)).Unix()
	return s.db.UpsertJob(context.Background(), store.Job{
		ID:        spec.ID(),
		Plugin:    spec.Plugin,
		Spec:      spec.Cron,
		Timezone:  s.tz.String(),
		NextRunAt: next,
		Payload:   spec.Payload,
	})
}

func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	s.started = true
	keep := make([]string, 0, len(s.specs))
	for id := range s.specs {
		keep = append(keep, id)
	}
	s.mu.Unlock()

	// 插件被删除后，其任务行保留但停用（保留执行历史）
	if err := s.db.DisableJobsExcept(ctx, keep); err != nil {
		return err
	}
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
		s.log.Warn("任务已无插件注册，停用", "job", j.ID)
		j.Enabled = false
		_ = s.db.UpdateJob(ctx, j)
		return
	}

	// 停机错过：标记 skipped 并推进，不补跑（避免重启雪崩）
	if now.Sub(time.Unix(j.NextRunAt, 0)) > missGrace {
		next := s.next(j.Spec, now)
		j.LastRunAt = now.Unix()
		j.LastStatus = "skipped"
		j.NextRunAt = next
		_ = s.db.UpdateJob(ctx, j)
		_ = s.db.RecordJobRun(ctx, j.ID, now.Unix(), now.Unix(), "skipped", "错过执行窗口")
		s.log.Warn("任务错过执行窗口，跳过", "job", j.ID)
		return
	}

	runner := s.runner(j.Plugin)
	if runner == nil {
		s.log.Error("插件未实现 JobRunner，停用任务", "job", j.ID)
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
	_ = s.db.RecordJobRun(ctx, j.ID, started.Unix(), finished.Unix(), status, errMsg)

	if err != nil {
		s.log.Error("任务执行失败", "job", j.ID, "status", status, "failures", failures, "err", err)
		if !enabled {
			s.log.Error("任务连续失败 5 次，已自动停用", "job", j.ID)
		}
		return
	}
	s.log.Info("任务执行成功", "job", j.ID, "cost", finished.Sub(started).String())
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

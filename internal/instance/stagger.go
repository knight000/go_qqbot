package instance

import (
	"context"
	"sync"
	"time"
)

// Stagger 把并发的「取网关地址」调用串成一条队列：同一次只放一个进去，
// 两次之间至少隔 Interval。
//
// 为什么需要它：`GET /gateway/bot` 限 2 QPM，而计量口径（按 AppID 还是按 IP）
// 平台没有说明。按 IP 的话，N 个 Bot 同时启动会互相挤配额，症状只是
// "偶尔启动失败"，没人能从那行日志反推出是这里。所以进程级共享一份，
// 默认 30s（对齐 2 QPM），由 `[bot].gateway_stagger` 配。
//
// 间隔从**上一次调用返回**开始算，不是从它开始算：一个慢请求（5s 超时）
// 之后紧接着发下一次，两次请求在平台侧就可能重叠，排队也就没意义了。
//
// 它**只服务启动**，由容器在启动循环走完时 Disarm：错峰要解决的是"N 条连接
// 别同时来抢地址"，不是"运行期每次重连都排队"。会话掉线后本来就要重连，
// 再压 30s 间隔等于把一次 4902 变成半分钟不可用。
type Stagger struct {
	interval time.Duration

	// off 在 Disarm 时关闭，之后 Do 直通。
	off  chan struct{}
	once sync.Once
	mu   sync.Mutex
	last time.Time
}

// NewStagger 返回一个按 interval 错峰的闸门；interval <= 0 表示只串行、
// 不额外等待（仍然保证同一时刻只有一次调用）。
func NewStagger(interval time.Duration) *Stagger {
	if interval < 0 {
		interval = 0
	}
	return &Stagger{interval: interval, off: make(chan struct{})}
}

// Interval 返回实际生效的间隔，供启动日志记录。
func (s *Stagger) Interval() time.Duration { return s.interval }

// Disarm 让闸门从此直通，可重复调用。
//
// 它不取 mu：调用点在启动循环末尾，而此刻可能有别的 Bot 正卡在间隔等待里。
// 取锁就得等那段等待结束（最多一个 interval），启动会被无谓地拉长。
func (s *Stagger) Disarm() {
	s.once.Do(func() { close(s.off) })
}

// Armed 报告闸门是否仍在错峰，供日志与测试断言。
func (s *Stagger) Armed() bool {
	select {
	case <-s.off:
		return false
	default:
		return true
	}
}

// Do 执行 fn：先等轮到自己，再等够间隔，最后整段持锁跑完。
//
// 持锁覆盖 fn 全程，而不是只覆盖"发号"：只发号的话，两个慢请求会重叠，
// "串行"就只剩名义。等待期间响应 ctx 与 Disarm，关停/启动结束能立刻退出
// 而不占着队列。
func (s *Stagger) Do(ctx context.Context, fn func(context.Context) error) error {
	if !s.Armed() {
		return fn(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 拿锁时可能已经被 Disarm：别再多等一个间隔。
	if !s.Armed() {
		return fn(ctx)
	}
	if !s.last.IsZero() {
		if wait := s.interval - time.Since(s.last); wait > 0 {
			t := time.NewTimer(wait)
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
				return ctx.Err()
			case <-s.off:
			}
		}
	}
	err := fn(ctx)
	s.last = time.Now()
	return err
}

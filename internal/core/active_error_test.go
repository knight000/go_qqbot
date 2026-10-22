package core

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

// 本文件是「主动消息失败可判别」的契约验收（#34）。插件只能 import host，
// 所以三种失败必须由 **host 的哨兵**表达，并由真实发送路径原样透出。
//
// 断言有两条不能妥协的写法：
//   - 期望值写 host.ErrX，不写 core 里的别名。别名是跟着实现走的：有人把
//     sender.go 改回各自 errors.New，别名仍然是那个新值，测试照样绿，而插件
//     侧的 errors.Is 已经全部失效——这正是 #34 描述的缺陷。
//   - 断言写成矩阵：命中的为真，另外两个必须为假。只判"命中"的话，三个哨兵
//     撞成同一个值也发现不了，而插件里的 if/else 链会同时进两个分支。

// quotaUsageStore 是 activeUsageStore 的假实现：只回答这一格能不能占到。
// ok=false 是「当日已满」，err!=nil 是「日预算账本坏了」——两者都该返回
// host.ErrActiveQuota，差别只在错误的上下文里。
type quotaUsageStore struct {
	ok  bool
	err error
}

func (f quotaUsageStore) TakeActiveBudget(context.Context, store.ActiveUsageKey, int) (bool, error) {
	return f.ok, f.err
}

func (f quotaUsageStore) RefundActiveBudget(context.Context, store.ActiveUsageKey) error { return nil }

// hostSentinel 是一个待验收的哨兵；name 只用于报错信息。
type hostSentinel struct {
	name string
	err  error
}

func hostSentinels() []hostSentinel {
	return []hostSentinel{
		{"ErrActiveQuota", host.ErrActiveQuota},
		{"ErrActiveDisabled", host.ErrActiveDisabled},
		{"ErrRateLimited", host.ErrRateLimited},
	}
}

// TestActiveErrorSentinelsAreDistinct 先钉住前提：三个哨兵若撞成同一个值，
// 下面的矩阵测试即使全绿也毫无意义。
func TestActiveErrorSentinelsAreDistinct(t *testing.T) {
	all := hostSentinels()
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[i].err == all[j].err {
				t.Fatalf("host.%s 与 host.%s 是同一个错误值，插件无法区分",
					all[i].name, all[j].name)
			}
		}
	}
}

func TestSenderActiveErrorsMatchHostSentinels(t *testing.T) {
	allowed := ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"probe": true},
		DailyPerPeer: 2,
	}

	cases := []struct {
		name   string
		policy ActivePolicy
		usage  quotaUsageStore
		api    *fakeAPI
		want   error // nil 表示三类哨兵一个都不该命中
	}{
		{
			name:   "日预算用尽",
			policy: allowed,
			usage:  quotaUsageStore{ok: false},
			api:    &fakeAPI{},
			want:   host.ErrActiveQuota,
		},
		{
			name:   "日预算账本不可用",
			policy: allowed,
			usage:  quotaUsageStore{err: errors.New("database is locked")},
			api:    &fakeAPI{},
			want:   host.ErrActiveQuota,
		},
		{
			name: "总开关关闭",
			policy: ActivePolicy{
				Plugins:      map[string]bool{"probe": true},
				DailyPerPeer: 2,
			},
			usage: quotaUsageStore{ok: true},
			api:   &fakeAPI{},
			want:  host.ErrActiveDisabled,
		},
		{
			name: "插件不在白名单",
			policy: ActivePolicy{
				Enabled:      true,
				Plugins:      map[string]bool{"someone-else": true},
				DailyPerPeer: 2,
			},
			usage: quotaUsageStore{ok: true},
			api:   &fakeAPI{},
			want:  host.ErrActiveDisabled,
		},
		{
			name:   "平台 429",
			policy: allowed,
			usage:  quotaUsageStore{ok: true},
			api:    &fakeAPI{rateLimitedAt: 1, retryAfter: time.Second},
			want:   host.ErrRateLimited,
		},
		{
			// 网络失败不是"预算/配置/限流"中的任何一种：插件该重试还是放弃
			// 由它自己按上下文决定，核心不冒充成三类之一。
			name:   "网络失败",
			policy: allowed,
			usage:  quotaUsageStore{ok: true},
			api:    &fakeAPI{failSendAt: 1},
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ForPlugin 是插件真正拿到的那个 Sender：白名单判定也在这一层。
			sender := NewSender(tc.api, slog.New(slog.DiscardHandler), tc.policy, tc.usage, time.UTC).
				ForPlugin("probe")

			err := sender.SendActive(context.Background(), host.SceneC2C, "u1", host.Reply{Content: "hi"})
			if err == nil {
				t.Fatal("这些场景都应返回错误")
			}
			for _, sn := range hostSentinels() {
				got := errors.Is(err, sn.err)
				want := tc.want == sn.err
				if got != want {
					t.Errorf("errors.Is(err, host.%s)=%v，期望 %v；err=%v", sn.name, got, want, err)
				}
			}
		})
	}
}

package core

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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

// TestActiveDisabledErrorNamesTheGate 是 #35 的验收：两条配置闸门的错误串
// 必须自己说清卡在哪一条。它们都映射到 host.ErrActiveDisabled，所以上面的
// 矩阵测试对"两条串完全同形"照样绿——这正是缺陷的藏身处：
// 总开关关闭时日志说"插件不在白名单"，运维照着改白名单，改完仍然发不出去。
//
// 错误串不是契约（§3.3 写明不得解析），正因如此它是唯一的排障线索，
// 所以这里直接断言串里的关键片段，而不是只看 errors.Is。
func TestActiveDisabledErrorNamesTheGate(t *testing.T) {
	cases := []struct {
		name string
		// policy 里的白名单在第一种场景下**包含**被测插件：总开关关掉时
		// 白名单里有没有它都无意义，错误串不该把运维指向白名单。
		policy        ActivePolicy
		plugin        string
		wantFragments []string
		// unwanted 断言两条串互不包含对方的特征片段，否则"分开了"只是
		// 换了个说法的同形串。
		unwanted string
	}{
		{
			name: "总开关关闭",
			policy: ActivePolicy{
				Enabled:      false,
				Plugins:      map[string]bool{"probe": true},
				DailyPerPeer: 2,
			},
			plugin:        "probe",
			wantFragments: []string{"[active].enabled", "c2c", "u1"},
			unwanted:      "白名单",
		},
		{
			name: "插件不在白名单",
			policy: ActivePolicy{
				Enabled:      true,
				Plugins:      map[string]bool{"someone-else": true},
				DailyPerPeer: 2,
			},
			plugin:        "digest",
			wantFragments: []string{"digest", "c2c", "u1", "白名单"},
			unwanted:      "[active].enabled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := NewSender(&fakeAPI{}, slog.New(slog.DiscardHandler), tc.policy,
				quotaUsageStore{ok: true}, time.UTC).ForPlugin(tc.plugin)

			err := sender.SendActive(context.Background(), host.SceneC2C, "u1", host.Reply{Content: "hi"})
			if err == nil {
				t.Fatal("被闸门挡下时应返回错误")
			}
			if !errors.Is(err, host.ErrActiveDisabled) {
				t.Fatalf("errors.Is(err, host.ErrActiveDisabled)=false；err=%v", err)
			}
			got := err.Error()
			for _, frag := range tc.wantFragments {
				if !strings.Contains(got, frag) {
					t.Errorf("错误串缺少 %q，看不出是哪一条闸门/哪个会话；err=%s", frag, got)
				}
			}
			if strings.Contains(got, tc.unwanted) {
				t.Errorf("错误串不应含 %q：两个原因被混在一起，运维会照着错的那条改；err=%s",
					tc.unwanted, got)
			}
		})
	}
}

// TestActiveDisabledErrorOnCoreSenderNamesPeer 覆盖 sendActiveOne 那条路径：
// 核心自己的群生命周期通知不经 ForPlugin，直接调 (*Sender).SendActive，
// 总开关关闭时曾经返回裸哨兵，日志里只剩"主动消息未开启"，没有场景也没有 peer。
func TestActiveDisabledErrorOnCoreSenderNamesPeer(t *testing.T) {
	s := NewSender(&fakeAPI{}, slog.New(slog.DiscardHandler), ActivePolicy{}, nil, time.UTC)

	err := s.SendActive(context.Background(), host.SceneGroup, "g1", host.Reply{Content: "hi"})
	if !errors.Is(err, host.ErrActiveDisabled) {
		t.Fatalf("主动消息总开关关闭时应拒绝，实际 err=%v", err)
	}
	got := err.Error()
	for _, frag := range []string{"[active].enabled", "group", "g1"} {
		if !strings.Contains(got, frag) {
			t.Errorf("错误串缺少 %q；err=%s", frag, got)
		}
	}
}

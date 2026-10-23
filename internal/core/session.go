package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// sessionBackend 把 store.DB 与 Router 适配成 host.SessionBackend。
// 单独一层与 adminStore 同理：host 不依赖 store，插件也拿不到数据库。
//
// 读到的行有四种含义：没有（建）、已过期（替换）、自己持有（刷新）、别人的
// （拒绝）。"过期"与"自己持有"两条判定的先后**不影响正确性**：ReplaceSession
// 是无条件覆盖，"自己持有 + 已过期"落在哪一条上都只是覆盖，走不到
// ErrSessionBusy。唯一实际差异是 created_at 是否重算，这里选过期优先——
// 过期即行不存在，行都不存在就没有"保留创建时刻"可言；created_at 记的是
// "这个 peer 最近一次被接管的时间"，跨过一次过期还留着旧值，排障时会读到
// 一个属于上一个会话的时间（ADR-0004 决策 4 / 决策 5 例外①）。
type sessionBackend struct {
	db     *store.DB
	router *Router
	log    *slog.Logger
}

// sessionExpired 是「过期等于行不存在」的唯一判据（ADR-0004 决策 4）。
// 写路径、消费路径、展示路径共用它，避免三处各写一遍边界判断。
func sessionExpired(expiresAt, now time.Time) bool { return !expiresAt.After(now) }

func (b sessionBackend) AwaitSession(ctx context.Context, plugin string, ev *host.Event, state []byte, ttl time.Duration) error {
	if b.db == nil {
		return fmt.Errorf("会话后端未初始化")
	}
	now := time.Now()
	next := store.Session{
		Peer:      ev.PeerID,
		Scene:     string(ev.Scene),
		Plugin:    plugin,
		State:     state,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}

	row, ok, err := b.db.Session(ctx, ev.PeerID)
	if err != nil {
		return err
	}
	if !ok {
		return b.create(ctx, next)
	}
	if sessionExpired(row.ExpiresAt, now) {
		// 过期等于行不存在：替换而不是拒绝，否则新插件要白等一个 TTL。
		// created_at 一并重算——上一个会话已经结束，它的接管时刻不再属于这一行。
		return b.replace(ctx, next)
	}
	if row.Plugin == plugin {
		// 自己持有 = "改 state、重新计时"，不是冲突。保留 created_at：
		// 排障要看的是"这个 peer 被占了多久"。
		ok, err := b.db.RefreshSession(ctx, ev.PeerID, plugin, state, next.ExpiresAt)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		// 行在读到写之间被删了或换了主人。回到创建路径再试一次——
		// 这一格是并发兜底，不是主路径，所以不循环重试。
		return b.create(ctx, next)
	}
	if b.router.Lookup(row.Plugin) == nil {
		// 持有者已卸载/重载后消失，没人会来消费这一行。删了再拒绝等于
		// 让新插件多等一条消息，所以直接替换。
		b.log.Warn("会话持有者已不在加载集合，按悬挂会话处理",
			"peer", ev.PeerID, "holder", row.Plugin, "plugin", plugin)
		return b.replace(ctx, next)
	}
	return host.ErrSessionBusy
}

func (b sessionBackend) create(ctx context.Context, s store.Session) error {
	err := b.db.CreateSession(ctx, s)
	// 主键冲突是并发兜底：两个 Await 同时读到"没有行"，只有一个能插进去。
	if errors.Is(err, store.ErrSessionExists) {
		return host.ErrSessionBusy
	}
	return err
}

func (b sessionBackend) replace(ctx context.Context, s store.Session) error {
	return b.db.ReplaceSession(ctx, s)
}

// sessionRows 把 store 的行类型收敛成 Dispatcher 需要的最小视图。
//
// 没有直接把 store.DB 塞进 Dispatcher：那会让 dispatch 依赖整个 store 包，
// 而它只需要"按 peer 读一行、按 peer 删一行"这两件事。
type sessionRows struct{ db *store.DB }

func (s sessionRows) Session(ctx context.Context, peer string) (sessionRow, bool, error) {
	row, ok, err := s.db.Session(ctx, peer)
	if err != nil || !ok {
		return sessionRow{}, ok, err
	}
	return sessionRow{
		Plugin:    row.Plugin,
		Scene:     row.Scene,
		State:     row.State,
		ExpiresAt: row.ExpiresAt,
	}, true, nil
}

func (s sessionRows) DeleteSession(ctx context.Context, peer string) error {
	return s.db.DeleteSession(ctx, peer)
}

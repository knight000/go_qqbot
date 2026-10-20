package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/knight000/go_qqbot/internal/host"
	"github.com/knight000/go_qqbot/internal/store"
)

// sessionBackend 把 store.DB 与 Router 适配成 host.SessionBackend。
// 单独一层与 adminStore 同理：host 不依赖 store，插件也拿不到数据库。
//
// 判定顺序即 ADR-0004 决策 5 的落地：读到的行有四种含义——没有（建）、
// 自己持有（刷新）、过期或悬挂（替换）、别人的（拒绝）。顺序不能换：
// 先判过期再判持有者会把"自己的行已过期"错当成冲突，先判持有者再判过期
// 则会让一个已过期的行挡住新插件。
type sessionBackend struct {
	db     *store.DB
	router *Router
	log    *slog.Logger
}

func (b sessionBackend) AwaitSession(ctx context.Context, plugin string, ev *host.Event, state []byte, ttl time.Duration) error {
	if b.db == nil {
		return fmt.Errorf("会话后端未初始化")
	}
	now := time.Now()
	expires := now.Add(ttl)

	row, ok, err := b.db.Session(ctx, ev.PeerID)
	if err != nil {
		return err
	}
	if !ok {
		return b.create(ctx, plugin, ev, state, now, expires)
	}
	if row.Plugin == plugin {
		// 自己持有 = "改 state、重新计时"，不是冲突。保留 created_at：
		// 它记的是最初接管时刻，排障时要看的是"这个 peer 被占了多久"。
		ok, err := b.db.RefreshSession(ctx, ev.PeerID, plugin, state, expires)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		// 行在读到写之间被删了或换了主人。回到创建路径再试一次——
		// 这一格是并发兜底，不是主路径，所以不循环重试。
		return b.create(ctx, plugin, ev, state, now, expires)
	}
	if !row.ExpiresAt.After(now) {
		// 过期等于行不存在：替换而不是拒绝，否则新插件要白等一个 TTL。
		return b.replace(ctx, plugin, ev, state, now, expires)
	}
	if b.router.Lookup(row.Plugin) == nil {
		// 持有者已卸载/重载后消失，没人会来消费这一行。删了再拒绝等于
		// 让新插件多等一条消息，所以直接替换。
		b.log.Warn("会话持有者已不在加载集合，按悬挂会话处理",
			"peer", ev.PeerID, "holder", row.Plugin, "plugin", plugin)
		return b.replace(ctx, plugin, ev, state, now, expires)
	}
	return host.ErrSessionBusy
}

func (b sessionBackend) create(ctx context.Context, plugin string, ev *host.Event, state []byte, created, expires time.Time) error {
	err := b.db.CreateSession(ctx, b.row(plugin, ev, state, created, expires))
	// 主键冲突是并发兜底：两个 Await 同时读到"没有行"，只有一个能插进去。
	if errors.Is(err, store.ErrSessionExists) {
		return host.ErrSessionBusy
	}
	return err
}

func (b sessionBackend) replace(ctx context.Context, plugin string, ev *host.Event, state []byte, created, expires time.Time) error {
	return b.db.ReplaceSession(ctx, b.row(plugin, ev, state, created, expires))
}

func (b sessionBackend) row(plugin string, ev *host.Event, state []byte, created, expires time.Time) store.Session {
	return store.Session{
		Peer:      ev.PeerID,
		Scene:     string(ev.Scene),
		Plugin:    plugin,
		State:     state,
		CreatedAt: created,
		ExpiresAt: expires,
	}
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

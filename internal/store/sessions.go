package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// sessions 是会话接管的唯一事实源（ADR-0004）：一行 = 一个 peer 当前被某个
// 插件占用。主键只有 peer，所以"同 peer 同时只有一个持有者"由数据库约束表达，
// 而不是靠调用方先查后写——两个并发 Await 都能通过检查，唯一约束不会。
//
// 过期等于行不存在：读路径据此判"有没有会话"，写路径把过期行当空闲行替换。
// 没有 expired 状态位，因为状态位与 expires_at 会在同一语义上打架。

// ErrSessionExists 表示该 peer 已被占用。store 层的哨兵，core 会把它翻成
// host.ErrSessionBusy——插件只 import host，看不到这个错误。
var ErrSessionExists = errors.New("store: 该 peer 已有会话持有者")

// Session 是 sessions 表的一行。
//
// Scene 是冗余列，只用于观测与消费校验：群 openid 与用户 openid 来自两个
// 平台命名空间，取值域不保证不相交，单聊消息不得消费群会话。
type Session struct {
	Peer      string
	Scene     string
	Plugin    string
	State     []byte
	CreatedAt time.Time
	ExpiresAt time.Time
}

// stateArg 把 nil 的 state 写成空串而不是 NULL：列是 NOT NULL，
// 而 Go 侧的零值就是 nil——插件不传 state 是合法用例，不该在运行时炸。
func stateArg(s []byte) []byte {
	if s == nil {
		return []byte{}
	}
	return s
}

// Session 按主键点查。第二个返回值是"行是否存在"，过期与否由调用方判定——
// store 不知道调用方的时间口径，也不该替它决定。
func (db *DB) Session(ctx context.Context, peer string) (Session, bool, error) {
	var (
		s       Session
		created int64
		expires int64
	)
	err := db.r.QueryRowContext(ctx,
		`SELECT peer, scene, plugin, state, created_at, expires_at FROM sessions WHERE peer = ?`,
		peer).Scan(&s.Peer, &s.Scene, &s.Plugin, &s.State, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("读取会话 %s: %w", peer, err)
	}
	s.CreatedAt = time.Unix(created, 0)
	s.ExpiresAt = time.Unix(expires, 0)
	return s, true, nil
}

// CreateSession 插入一行新会话。peer 已被占用时返回 ErrSessionExists。
//
// 用一条 ON CONFLICT DO NOTHING 而不是"先查再插"：后者在并发下两个调用都能
// 通过检查，前者把冲突判定交给主键。也不用解析驱动返回的唯一约束错误串——
// 那是把实现细节写进错误分类里。
func (db *DB) CreateSession(ctx context.Context, s Session) error {
	created := s.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	res, err := db.w.ExecContext(ctx,
		`INSERT INTO sessions(peer, scene, plugin, state, created_at, expires_at)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(peer) DO NOTHING`,
		s.Peer, s.Scene, s.Plugin, stateArg(s.State), created.Unix(), s.ExpiresAt.Unix())
	if err != nil {
		return fmt.Errorf("创建会话 %s: %w", s.Peer, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("创建会话 %s: %w", s.Peer, err)
	}
	if n == 0 {
		return ErrSessionExists
	}
	return nil
}

// RefreshSession 只更新持有者自己的行：改 state、重新计时，保留 created_at。
// 行不存在或持有者不是 plugin 时返回 false，调用方据此区分"该重试"与"该报忙"。
//
// 用 UPDATE ... WHERE 而不是先读后写：读到的持有者在写之前可能已经变了，
// 而 WHERE 命中与否由数据库在写锁内判定。
func (db *DB) RefreshSession(ctx context.Context, peer, plugin string, state []byte, expiresAt time.Time) (bool, error) {
	res, err := db.w.ExecContext(ctx,
		`UPDATE sessions SET state = ?, expires_at = ? WHERE peer = ? AND plugin = ?`,
		stateArg(state), expiresAt.Unix(), peer, plugin)
	if err != nil {
		return false, fmt.Errorf("刷新会话 %s: %w", peer, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("刷新会话 %s: %w", peer, err)
	}
	return n > 0, nil
}

// ReplaceSession 无条件覆盖一行，服务两个场景：过期行（语义上等于不存在）
// 与悬挂行（持有者已卸载，没人会来消费它）。两者都不算冲突。
func (db *DB) ReplaceSession(ctx context.Context, s Session) error {
	created := s.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	if _, err := db.w.ExecContext(ctx,
		`INSERT OR REPLACE INTO sessions(peer, scene, plugin, state, created_at, expires_at)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		s.Peer, s.Scene, s.Plugin, stateArg(s.State), created.Unix(), s.ExpiresAt.Unix()); err != nil {
		return fmt.Errorf("替换会话 %s: %w", s.Peer, err)
	}
	return nil
}

// DeleteSession 删一行。删 0 行不是错误：消费路径要把"行已经不在了"
// 与"删失败"分开，只有后者该打断投递。
func (db *DB) DeleteSession(ctx context.Context, peer string) error {
	if _, err := db.w.ExecContext(ctx, `DELETE FROM sessions WHERE peer = ?`, peer); err != nil {
		return fmt.Errorf("删除会话 %s: %w", peer, err)
	}
	return nil
}

// PurgeExpiredSessions 清过期行，随 GC 兜底跑。读路径已经会删过期行，
// 这里只处理"再也没有人来读过"的那些。
func (db *DB) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := db.w.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, nowUnix())
	if err != nil {
		return 0, fmt.Errorf("清理过期会话: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountSessions 是"有多少 peer 正处于会话中"，供 /group status 总览用。
func (db *DB) CountSessions(ctx context.Context) (int64, error) {
	var n int64
	if err := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计会话: %w", err)
	}
	return n, nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 群名单模式。三态而不是两个布尔列，是为了让"同时在黑白名单"这种
// 自相矛盾的状态不可能出现：一份授权只能属于一个名单。
const (
	GroupModeNone  = ""      // 未设置：白名单开启时会被拦下
	GroupModeAllow = "allow" // 白名单
	GroupModeDeny  = "deny"  // 黑名单（优先于白名单）
)

// Group 是"见过的群"的登记项。
//
// 记录只增不删：机器人退群时打 LeftAt 标记，而不是删行。删掉的话
// "这个群曾经被授权过""它以前叫什么"这些信息就永久丢失了，
// 而群名与名单恰恰是退群后最需要回看的诊断线索。
type Group struct {
	OpenID     string
	Name       string
	NameSource string // api | manual | ''
	Mode       string
	// LeftAt 非零表示机器人已不在该群，值是退出时间。
	LeftAt time.Time
	// FirstSeenAt 是第一次收到该群事件的时间；LastSeenAt 是最近一次。
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

func (g Group) Allowed() bool { return g.Mode == GroupModeAllow }
func (g Group) Blocked() bool { return g.Mode == GroupModeDeny }

// Left 表示机器人已不在该群（收到退群事件，且之后没有新的群消息）。
func (g Group) Left() bool { return !g.LeftAt.IsZero() }

// GroupStats 是群列表的统计口径。Left 与 total 是包含关系：
// total 是登记总数，Left 是其中"机器人已退出"的部分。
type GroupStats struct {
	Total   int
	Allowed int
	Denied  int
	Left    int
}

// InUse 是在群数量（total 去掉已退出的）。
func (s GroupStats) InUse() int { return s.Total - s.Left }

// groupColumns 与 scanGroup 成对维护：加列时只改这两处，
// 否则四五个 Scan 点会漏掉一个，症状是字段永远是零值。
const groupColumns = `group_openid, group_name, name_source, mode, left_at,
	first_seen_at, last_seen_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanGroup(sc rowScanner) (Group, error) {
	var (
		g                 Group
		left, first, last int64
	)
	if err := sc.Scan(&g.OpenID, &g.Name, &g.NameSource, &g.Mode, &left, &first, &last); err != nil {
		return Group{}, err
	}
	if left > 0 {
		g.LeftAt = time.Unix(left, 0)
	}
	g.FirstSeenAt = time.Unix(first, 0)
	g.LastSeenAt = time.Unix(last, 0)
	return g, nil
}

// ObserveGroup 登记一次群事件，返回当前登记项以及状态是否发生了变化。
//
// changed = 首次见到该群，或它刚从"已退出"恢复成"在群"。两个时机都需要
// 调用方做一次性的补建动作（取群名、展开 per-peer 任务实例），
// 而普通的"又来了一条消息"不应该触发任何额外写入。
//
// 只更新 last_seen_at，不碰名称与名单：名称由首次取群名或 /group name 写入，
// 名单只能由管理员显式修改——收到消息绝不能顺带把群加进白名单。
//
// 顺带清掉 left_at：能收到消息就说明机器人在群里，比任何事件通知都可信。
// 这样即便 GROUP_DEL_ROBOT / GROUP_ADD_ROBOT 在断线期间丢失，状态也会自愈。
func (db *DB) ObserveGroup(ctx context.Context, openid string) (Group, bool, error) {
	if openid == "" {
		return Group{}, false, errors.New("群 openid 为空")
	}
	now := nowUnix()
	res, err := db.w.ExecContext(ctx,
		`INSERT INTO qq_groups(group_openid, first_seen_at, last_seen_at, updated_at)
		 VALUES(?, ?, ?, ?)
		 ON CONFLICT(group_openid) DO NOTHING`,
		openid, now, now, now)
	if err != nil {
		return Group{}, false, fmt.Errorf("登记群: %w", err)
	}
	n, _ := res.RowsAffected()
	changed := n > 0
	if !changed {
		// 先看退出标记再更新：更新之后 left_at 已归零，就看不出它曾经是退出的。
		var wasLeft int64
		if err := db.r.QueryRowContext(ctx,
			`SELECT left_at FROM qq_groups WHERE group_openid = ?`, openid).Scan(&wasLeft); err != nil {
			return Group{}, false, fmt.Errorf("读取群退出标记: %w", err)
		}
		changed = wasLeft != 0

		// 活跃时间按分钟粒度更新即可：群消息可能很密集，没必要每条都写一次。
		// 但 left_at 的清零不受粒度限制——那是个状态修正，写一次就够。
		if _, err := db.w.ExecContext(ctx,
			`UPDATE qq_groups
			    SET last_seen_at = CASE WHEN last_seen_at < ? THEN ? ELSE last_seen_at END,
			        left_at      = 0,
			        updated_at   = ?
			  WHERE group_openid = ? AND (last_seen_at < ? OR left_at <> 0)`,
			now-60, now, now, openid, now-60); err != nil {
			return Group{}, false, fmt.Errorf("更新群活跃时间: %w", err)
		}
	}
	g, ok, err := db.GetGroup(ctx, openid)
	if err != nil {
		return Group{}, false, err
	}
	if !ok {
		return Group{}, false, fmt.Errorf("群 %s 登记后读取失败", openid)
	}
	return g, changed, nil
}

// GetGroup 读取一个群的登记项。
func (db *DB) GetGroup(ctx context.Context, openid string) (Group, bool, error) {
	g, err := scanGroup(db.r.QueryRowContext(ctx,
		`SELECT `+groupColumns+` FROM qq_groups WHERE group_openid = ?`, openid))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Group{}, false, nil
		}
		return Group{}, false, fmt.Errorf("读取群 %s: %w", openid, err)
	}
	return g, true, nil
}

// MarkGroupPresent 撤销退出标记（机器人重新入群）。
// 群不在登记表里时顺带建档：入群事件是比"第一条消息"更早的登记时机。
func (db *DB) MarkGroupPresent(ctx context.Context, openid string) error {
	if openid == "" {
		return errors.New("群 openid 为空")
	}
	now := nowUnix()
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO qq_groups(group_openid, left_at, first_seen_at, last_seen_at, updated_at)
		 VALUES(?, 0, ?, ?, ?)
		 ON CONFLICT(group_openid) DO UPDATE SET
		   left_at      = 0,
		   last_seen_at = excluded.last_seen_at,
		   updated_at   = excluded.updated_at`,
		openid, now, now, now); err != nil {
		return fmt.Errorf("标记群在群: %w", err)
	}
	return nil
}

// MarkGroupLeft 标记机器人已退出该群。保留名单与名称——重新拉回群时
// 白名单还在，不需要管理员再授权一次。
func (db *DB) MarkGroupLeft(ctx context.Context, openid string) error {
	if openid == "" {
		return errors.New("群 openid 为空")
	}
	now := nowUnix()
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO qq_groups(group_openid, left_at, first_seen_at, last_seen_at, updated_at)
		 VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(group_openid) DO UPDATE SET
		   left_at    = excluded.left_at,
		   updated_at = excluded.updated_at`,
		openid, now, now, now, now); err != nil {
		return fmt.Errorf("标记群退出: %w", err)
	}
	return nil
}

// SetGroupMode 设置群名单模式。群不在登记表里时顺带建档，
// 这样管理员可以提前把还没说过话的群加进白名单。
func (db *DB) SetGroupMode(ctx context.Context, openid, mode string) error {
	switch mode {
	case GroupModeNone, GroupModeAllow, GroupModeDeny:
	default:
		return fmt.Errorf("非法的群名单模式 %q", mode)
	}
	now := nowUnix()
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO qq_groups(group_openid, mode, first_seen_at, last_seen_at, updated_at)
		 VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(group_openid) DO UPDATE SET
		   mode       = excluded.mode,
		   updated_at = excluded.updated_at`,
		openid, mode, now, now, now); err != nil {
		return fmt.Errorf("设置群名单: %w", err)
	}
	return nil
}

// SetGroupName 更新群名称。source 为 manual 时覆盖一切；
// 自动取名（source=api）不得覆盖管理员手工设置的名字。
func (db *DB) SetGroupName(ctx context.Context, openid, name, source string) error {
	now := nowUnix()
	manual := 0
	if source == "manual" {
		manual = 1
	}
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO qq_groups(group_openid, group_name, name_source, first_seen_at, last_seen_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(group_openid) DO UPDATE SET
		   group_name = CASE
		     WHEN qq_groups.name_source = 'manual' AND ? = 0 THEN qq_groups.group_name
		     ELSE excluded.group_name END,
		   name_source = CASE
		     WHEN qq_groups.name_source = 'manual' AND ? = 0 THEN qq_groups.name_source
		     ELSE excluded.name_source END,
		   updated_at = excluded.updated_at`,
		openid, name, source, now, now, now, manual, manual); err != nil {
		return fmt.Errorf("设置群名称: %w", err)
	}
	return nil
}

// ListGroups 列出全部已登记的群：白名单在前，其次黑名单，最后未设置；
// 同组内按最近活跃倒序。已退出的群一律沉到最后——它们只供回看。
func (db *DB) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT `+groupColumns+`
		 FROM qq_groups
		 ORDER BY left_at <> 0,
		          CASE mode WHEN 'allow' THEN 0 WHEN 'deny' THEN 1 ELSE 2 END,
		          last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("列出群: %w", err)
	}
	defer rows.Close()

	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("解析群行: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListGroupsInUse 列出机器人当前仍在的群（left_at = 0），按最近活跃倒序。
// 批量刷新群名用它——已退出的群调接口只会拿到错误。
func (db *DB) ListGroupsInUse(ctx context.Context) ([]Group, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT `+groupColumns+`
		 FROM qq_groups
		 WHERE left_at = 0
		 ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("列出在群: %w", err)
	}
	defer rows.Close()

	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("解析群行: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CountGroups 返回登记总数、两个名单的数量与已退出的数量，
// 用于 /group status 与启动日志。
func (db *DB) CountGroups(ctx context.Context) (GroupStats, error) {
	var s GroupStats
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN mode = 'allow' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN mode = 'deny'  THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN left_at <> 0     THEN 1 ELSE 0 END), 0)
		 FROM qq_groups`).Scan(&s.Total, &s.Allowed, &s.Denied, &s.Left); err != nil {
		return GroupStats{}, fmt.Errorf("统计群: %w", err)
	}
	return s, nil
}

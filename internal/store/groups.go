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

// Group 是"见过的群"的登记项。平台没有"列出机器人加入的群"的接口，
// 这张表只能靠收到群事件时逐步积累。
type Group struct {
	OpenID     string
	Name       string
	NameSource string // api | manual | ''
	Mode       string
	// FirstSeenAt 是第一次收到该群事件的时间；LastSeenAt 是最近一次。
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

func (g Group) Allowed() bool { return g.Mode == GroupModeAllow }
func (g Group) Blocked() bool { return g.Mode == GroupModeDeny }

// ObserveGroup 登记一次群事件，返回当前登记项以及是否首次见到该群。
//
// 只更新 last_seen_at，不碰名称与名单：名称由首次取群名或 /group name 写入，
// 名单只能由管理员显式修改——收到消息绝不能顺带把群加进白名单。
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
	isNew := n > 0
	if !isNew {
		// 活跃时间按分钟粒度更新即可：群消息可能很密集，没必要每条都写一次。
		if _, err := db.w.ExecContext(ctx,
			`UPDATE qq_groups SET last_seen_at = ?
			 WHERE group_openid = ? AND last_seen_at < ?`,
			now, openid, now-60); err != nil {
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
	return g, isNew, nil
}

// GetGroup 读取一个群的登记项。
func (db *DB) GetGroup(ctx context.Context, openid string) (Group, bool, error) {
	var (
		g           Group
		first, last int64
	)
	err := db.r.QueryRowContext(ctx,
		`SELECT group_openid, group_name, name_source, mode, first_seen_at, last_seen_at
		 FROM qq_groups WHERE group_openid = ?`, openid).
		Scan(&g.OpenID, &g.Name, &g.NameSource, &g.Mode, &first, &last)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Group{}, false, nil
		}
		return Group{}, false, fmt.Errorf("读取群 %s: %w", openid, err)
	}
	g.FirstSeenAt = time.Unix(first, 0)
	g.LastSeenAt = time.Unix(last, 0)
	return g, true, nil
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
// 同组内按最近活跃倒序。
func (db *DB) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT group_openid, group_name, name_source, mode, first_seen_at, last_seen_at
		 FROM qq_groups
		 ORDER BY CASE mode WHEN 'allow' THEN 0 WHEN 'deny' THEN 1 ELSE 2 END,
		          last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("列出群: %w", err)
	}
	defer rows.Close()

	var out []Group
	for rows.Next() {
		var (
			g           Group
			first, last int64
		)
		if err := rows.Scan(&g.OpenID, &g.Name, &g.NameSource, &g.Mode, &first, &last); err != nil {
			return nil, fmt.Errorf("解析群行: %w", err)
		}
		g.FirstSeenAt = time.Unix(first, 0)
		g.LastSeenAt = time.Unix(last, 0)
		out = append(out, g)
	}
	return out, rows.Err()
}

// CountGroups 返回登记总数与两个名单的数量，用于 /group status 的概览。
func (db *DB) CountGroups(ctx context.Context) (total, allowed, denied int, err error) {
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN mode = 'allow' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN mode = 'deny'  THEN 1 ELSE 0 END), 0)
		 FROM qq_groups`).Scan(&total, &allowed, &denied); err != nil {
		return 0, 0, 0, fmt.Errorf("统计群: %w", err)
	}
	return total, allowed, denied, nil
}

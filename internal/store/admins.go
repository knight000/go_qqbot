package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// sceneGroup 与 host.SceneGroup 保持一致；store 不依赖 host，这里单独定义。
const sceneGroup = "group"

// 角色只有两级：
//   - owner：认领产生，唯一能增删 admin；
//   - admin：辅助管理员，除"增删管理员"外与 owner 同权限。
const (
	RoleOwner = "owner"
	RoleAdmin = "admin"
)

// Admin 是一条管理员授权。peer_scope 为空表示该场景下全局有效。
type Admin struct {
	OpenID    string
	UnionID   string
	Scene     string
	PeerScope string
	Role      string
	Note      string
	GrantedBy string
	CreatedAt time.Time
}

// AdminTarget 是启动通知的收件人。C2COpenID 为空表示该管理员只有群身份，
// 无法主动私聊（平台单聊接口只接受单聊场景的 openid）。
type AdminTarget struct {
	Role      string
	C2COpenID string
	Note      string
}

// CountAdmins 用于启动时判断是否需要打印认领令牌。
func (db *DB) CountAdmins(ctx context.Context) (int, error) {
	var n int
	if err := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计管理员: %w", err)
	}
	return n, nil
}

// UpsertAdmin 写入/覆盖一条授权。空 note / union 不会覆盖已有值，
// 避免一次简写把之前记录的备注与身份映射清掉。
func (db *DB) UpsertAdmin(ctx context.Context, a Admin) error {
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO admins(openid, scene, peer_scope, role, granted_by, note, union_openid, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(openid, scene, peer_scope) DO UPDATE SET
		   role         = excluded.role,
		   granted_by   = excluded.granted_by,
		   note         = CASE WHEN excluded.note <> '' THEN excluded.note ELSE admins.note END,
		   union_openid = CASE WHEN excluded.union_openid <> '' THEN excluded.union_openid ELSE admins.union_openid END`,
		a.OpenID, a.Scene, a.PeerScope, a.Role, a.GrantedBy, a.Note, a.UnionID, nowUnix())
	if err != nil {
		return fmt.Errorf("写入管理员: %w", err)
	}
	return nil
}

// AdminRole 查询某人在某场景/某 peer 下的最高角色。
//
// 身份匹配三种方式（任一命中即可）：
//  1. 本场景 openid 直接命中；
//  2. 该条授权显式记录了 union_openid；
//  3. 平台给出的 union_openid 与本人在 identities 里的其它身份一致
//     （在群里认领/被添加的人，私聊里同样算管理员）。
//
// 作用域规则：
//   - owner 全局有效（认领即最高权限）；
//   - admin 在群场景只认"群内添加"的授权，且 peer_scope 必须匹配当前群；
//     私聊场景认所有授权——在群里加的 admin 可以私聊管理机器人，
//     但私聊加的 admin 不会自动获得群内权限。
func (db *DB) AdminRole(ctx context.Context, openid, unionID, scene, peer string) (role, scope string, ok bool, err error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT role, peer_scope, scene FROM admins
		 WHERE (openid = ? AND scene = ?)
		    OR (? <> '' AND union_openid = ?)
		    OR (? <> '' AND openid IN (SELECT openid FROM identities WHERE union_openid = ?))
		 ORDER BY CASE role WHEN 'owner' THEN 0 ELSE 1 END`,
		openid, scene, unionID, unionID, unionID, unionID)
	if err != nil {
		return "", "", false, fmt.Errorf("查询管理员: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r, s, rowScene string
		if err := rows.Scan(&r, &s, &rowScene); err != nil {
			return "", "", false, fmt.Errorf("解析管理员: %w", err)
		}
		if r != RoleOwner && scene == sceneGroup {
			if rowScene != sceneGroup {
				continue // 私聊里加的 admin 不扩散到群
			}
			if s != "" && s != peer {
				continue // 该授权属于别的群
			}
		}
		return r, s, true, nil
	}
	if err := rows.Err(); err != nil {
		return "", "", false, fmt.Errorf("遍历管理员: %w", err)
	}
	return "", "", false, nil
}

// ListAdmins 返回全部授权，owner 在前。
func (db *DB) ListAdmins(ctx context.Context) ([]Admin, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT openid, union_openid, scene, peer_scope, role, note, COALESCE(granted_by, ''), created_at
		 FROM admins
		 ORDER BY CASE role WHEN 'owner' THEN 0 ELSE 1 END, created_at`)
	if err != nil {
		return nil, fmt.Errorf("列出管理员: %w", err)
	}
	defer rows.Close()

	var out []Admin
	for rows.Next() {
		var a Admin
		var ts int64
		if err := rows.Scan(&a.OpenID, &a.UnionID, &a.Scene, &a.PeerScope,
			&a.Role, &a.Note, &a.GrantedBy, &ts); err != nil {
			return nil, fmt.Errorf("解析管理员行: %w", err)
		}
		a.CreatedAt = time.Unix(ts, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAdmin 删除某个标识对应的 admin 授权，owner 永远不动。
// 返回实际删除的行数，以及被拒绝删除的 owner 行数（用于给用户准确提示）。
func (db *DB) DeleteAdmin(ctx context.Context, ident string) (removed, owners int64, err error) {
	openid, union := db.ResolveIdent(ctx, ident, "", "")
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM admins
		 WHERE role = 'owner' AND (openid = ? OR openid = ? OR (? <> '' AND union_openid = ?))`,
		ident, openid, union, union).Scan(&owners); err != nil {
		return 0, 0, fmt.Errorf("统计 owner: %w", err)
	}
	res, err := db.w.ExecContext(ctx,
		`DELETE FROM admins
		 WHERE role <> 'owner' AND (openid = ? OR openid = ? OR (? <> '' AND union_openid = ?))`,
		ident, openid, union, union)
	if err != nil {
		return 0, owners, fmt.Errorf("删除管理员: %w", err)
	}
	removed, _ = res.RowsAffected()
	return removed, owners, nil
}

// AdminTargets 汇总启动通知的收件人：同一人的多条授权合并为一条，
// 角色取最高，并尽量解析出可用的单聊 openid。
func (db *DB) AdminTargets(ctx context.Context) ([]AdminTarget, error) {
	admins, err := db.ListAdmins(ctx)
	if err != nil {
		return nil, err
	}
	if len(admins) == 0 {
		return nil, nil
	}

	rows, err := db.r.QueryContext(ctx,
		`SELECT union_openid, openid FROM identities WHERE scene = 'c2c' AND union_openid <> ''`)
	if err != nil {
		return nil, fmt.Errorf("读取单聊身份: %w", err)
	}
	defer rows.Close()
	c2cByUnion := make(map[string]string)
	for rows.Next() {
		var union, openid string
		if err := rows.Scan(&union, &openid); err != nil {
			return nil, fmt.Errorf("解析单聊身份: %w", err)
		}
		c2cByUnion[union] = openid
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seen := make(map[string]int, len(admins))
	var out []AdminTarget
	for _, a := range admins {
		c2c := ""
		switch {
		case a.Scene == "c2c":
			c2c = a.OpenID
		case a.UnionID != "":
			c2c = c2cByUnion[a.UnionID]
		}
		key := c2c
		if key == "" {
			if a.UnionID != "" {
				key = "union:" + a.UnionID
			} else {
				key = "openid:" + a.OpenID
			}
		}
		if i, ok := seen[key]; ok {
			if a.Role == RoleOwner {
				out[i].Role = RoleOwner
			}
			if out[i].Note == "" {
				out[i].Note = a.Note
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, AdminTarget{Role: a.Role, C2COpenID: c2c, Note: a.Note})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Role == RoleOwner && out[j].Role != RoleOwner
	})
	return out, nil
}

// --- 重置审计 ---

// AuditReset 记录重置尝试。重置不可逆，审计表不可省略（§8.3）。
func (db *DB) AuditReset(ctx context.Context, actor, peer, level, detail string) error {
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO reset_audit(actor, peer, level, detail, created_at) VALUES(?, ?, ?, ?, ?)`,
		actor, peer, level, detail, nowUnix()); err != nil {
		return fmt.Errorf("写入重置审计: %w", err)
	}
	return nil
}

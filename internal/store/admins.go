package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// 角色定义与 host.Role 对齐（store 不依赖 host，这里单独定义）：
//   - owner：认领产生，唯一，能增删 admin；
//   - admin：辅助管理员，除"增删管理员"外与 owner 同权限。
//
// 两者都是**全局角色**：openid 跨场景同值（平台 author.id），在群里添加
// 与私聊添加效果一致，不再有 peer_scope 那种"只在某个群生效"的委托。
//
// 三级中的 user 不是 admins 表的值：查不到授权的自然状态就是普通用户。
const (
	RoleOwner = "owner"
	RoleAdmin = "admin"
)

// Admin 是一条管理员授权。SourceScene 只记录"在群聊还是私聊里添加的"，
// 用于展示来源，不参与权限判定。
type Admin struct {
	OpenID      string
	UnionID     string
	Role        string
	Note        string
	GrantedBy   string
	SourceScene string
	CreatedAt   time.Time
}

// AdminTarget 是启动通知的收件人。C2COpenID 为空表示没有可用的单聊地址。
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

// UpsertAdmin 写入/覆盖一条授权。空 note / union / source 不会覆盖已有值，
// 避免一次简写把之前记录的备注与身份映射清掉。
//
// owner 不会被降级：/admin 不提供任何降级路径，upsert 也不该成为后门。
func (db *DB) UpsertAdmin(ctx context.Context, a Admin) error {
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO admins(openid, union_openid, role, note, granted_by, source_scene, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(openid) DO UPDATE SET
		   role         = CASE WHEN admins.role = 'owner' THEN 'owner' ELSE excluded.role END,
		   granted_by   = excluded.granted_by,
		   source_scene = CASE WHEN excluded.source_scene <> '' THEN excluded.source_scene ELSE admins.source_scene END,
		   note         = CASE WHEN excluded.note <> '' THEN excluded.note ELSE admins.note END,
		   union_openid = CASE WHEN excluded.union_openid <> '' THEN excluded.union_openid ELSE admins.union_openid END`,
		a.OpenID, a.UnionID, a.Role, a.Note, a.GrantedBy, a.SourceScene, nowUnix())
	if err != nil {
		return fmt.Errorf("写入管理员: %w", err)
	}
	return nil
}

// AdminRole 查询某人的 Bot 角色。
//
// openid 是平台给出的用户唯一标识（author.id），群聊与单聊对同一个人取同值，
// 因此一次匹配即可覆盖所有场景——这正是"群里加的 admin 私聊也生效"的依据。
// union_openid 只作兜底：部分事件版本会下发它，能认出同一个人。
//
// 返回 ok=false 表示不是管理员。库故障返回 error，调用方按 fail-closed 处理。
func (db *DB) AdminRole(ctx context.Context, openid, unionID string) (role string, ok bool, err error) {
	if openid == "" && unionID == "" {
		return "", false, nil
	}

	const order = ` ORDER BY CASE role WHEN 'owner' THEN 0 ELSE 1 END LIMIT 1`
	var row *sql.Row
	if unionID == "" {
		row = db.r.QueryRowContext(ctx,
			`SELECT role FROM admins WHERE openid = ?`+order, openid)
	} else {
		row = db.r.QueryRowContext(ctx,
			`SELECT role FROM admins
			 WHERE openid = ?
			    OR (union_openid <> '' AND union_openid = ?)
			    OR openid IN (SELECT openid FROM identities WHERE union_openid = ?)`+order,
			openid, unionID, unionID)
	}

	if err := row.Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("查询管理员: %w", err)
	}
	return role, true, nil
}

// ListAdmins 返回全部授权，owner 在前。
func (db *DB) ListAdmins(ctx context.Context) ([]Admin, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT openid, union_openid, role, note, COALESCE(granted_by, ''), source_scene, created_at
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
		if err := rows.Scan(&a.OpenID, &a.UnionID, &a.Role, &a.Note,
			&a.GrantedBy, &a.SourceScene, &ts); err != nil {
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

// AdminTargets 汇总启动通知的收件人，尽量解析出可用的单聊 openid。
//
// 解析顺序：identities 里记录的单聊身份 → 授权自身的 openid（openid 跨场景同值，
// 实测如此）。解析错了只会让主动私聊失败并留下一条 warn，不会误发给别人。
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

	out := make([]AdminTarget, 0, len(admins))
	for _, a := range admins {
		c2c := a.OpenID
		if a.UnionID != "" && c2cByUnion[a.UnionID] != "" {
			c2c = c2cByUnion[a.UnionID]
		}
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

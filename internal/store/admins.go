package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CountAdmins 用于启动时判断是否需要打印认领令牌。
func (db *DB) CountAdmins(ctx context.Context) (int, error) {
	var n int
	if err := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计管理员: %w", err)
	}
	return n, nil
}

// AddAdmin 写入/覆盖一条授权。peer_scope 为空串表示全局。
func (db *DB) AddAdmin(ctx context.Context, openid, scene, scope, role, grantedBy string) error {
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO admins(openid, scene, peer_scope, role, granted_by, created_at)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(openid, scene, peer_scope)
		 DO UPDATE SET role = excluded.role, granted_by = excluded.granted_by`,
		openid, scene, scope, role, grantedBy, nowUnix())
	if err != nil {
		return fmt.Errorf("写入管理员: %w", err)
	}
	return nil
}

// AdminRole 查询某人在某场景/某 peer 下的最高角色。
// owner 隐含全部权限，因此排序把 owner 放前面。
func (db *DB) AdminRole(ctx context.Context, openid, scene, peer string) (role, scope string, ok bool, err error) {
	err = db.r.QueryRowContext(ctx,
		`SELECT role, peer_scope FROM admins
		 WHERE openid = ? AND scene = ? AND (peer_scope = '' OR peer_scope = ?)
		 ORDER BY CASE role WHEN 'owner' THEN 0 ELSE 1 END
		 LIMIT 1`,
		openid, scene, peer).Scan(&role, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("查询管理员: %w", err)
	}
	return role, scope, true, nil
}

func (db *DB) DeleteAdmin(ctx context.Context, openid, scene, scope string) error {
	if _, err := db.w.ExecContext(ctx,
		`DELETE FROM admins WHERE openid = ? AND scene = ? AND peer_scope = ?`,
		openid, scene, scope); err != nil {
		return fmt.Errorf("删除管理员: %w", err)
	}
	return nil
}

// AuditReset 记录重置尝试。重置不可逆，审计表不可省略（§8.3）。
func (db *DB) AuditReset(ctx context.Context, actor, peer, level, detail string) error {
	if _, err := db.w.ExecContext(ctx,
		`INSERT INTO reset_audit(actor, peer, level, detail, created_at) VALUES(?, ?, ?, ?, ?)`,
		actor, peer, level, detail, nowUnix()); err != nil {
		return fmt.Errorf("写入重置审计: %w", err)
	}
	return nil
}

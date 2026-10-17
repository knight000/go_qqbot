package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Identity 是某个用户在一个场景下的身份记录。
// 平台不提供 QQ 号：群成员是 member_openid，单聊是 user_openid，
// 两者不可互换；union_openid 是平台给出的跨场景唯一标识。
type Identity struct {
	OpenID    string
	UnionID   string
	Scene     string
	PeerScope string
}

// LinkIdentity 记录 openid 与 union_openid 的对应关系。
// 每条消息都调用一次，代价是一条小 upsert；只有平台给了 union 才有意义。
func (db *DB) LinkIdentity(ctx context.Context, openid, unionID, scene, peerScope string) error {
	if openid == "" || unionID == "" {
		return nil
	}
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO identities(openid, scene, peer_scope, union_openid, last_seen)
		 VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(openid, scene, peer_scope) DO UPDATE SET
		   union_openid = excluded.union_openid,
		   last_seen    = excluded.last_seen`,
		openid, scene, peerScope, unionID, nowUnix())
	if err != nil {
		return fmt.Errorf("记录用户身份: %w", err)
	}
	return nil
}

// ResolveIdentity 把用户输入的标识（openid 或 union_openid）解析成一条已知身份。
// 优先匹配当前场景/范围的那条记录，找不到就返回 false，由调用方按"手写 openid"处理。
func (db *DB) ResolveIdentity(ctx context.Context, ident, scene, peer string) (Identity, bool, error) {
	if ident == "" {
		return Identity{}, false, nil
	}

	var id Identity
	err := db.r.QueryRowContext(ctx,
		`SELECT openid, union_openid, scene, peer_scope FROM identities
		 WHERE (openid = ? OR (union_openid <> '' AND union_openid = ?))
		 ORDER BY (scene = ? AND peer_scope = ?) DESC, last_seen DESC
		 LIMIT 1`,
		ident, ident, scene, peer).Scan(&id.OpenID, &id.UnionID, &id.Scene, &id.PeerScope)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Identity{}, false, nil
		}
		return Identity{}, false, fmt.Errorf("解析用户身份 %s: %w", ident, err)
	}
	return id, true, nil
}

// ResolveIdent 是 ResolveIdentity 的宽松版：解析失败只返回空值，
// 用于删除这类"解析不到也应该继续按字面匹配"的场景。
func (db *DB) ResolveIdent(ctx context.Context, ident, scene, peer string) (openid, union string) {
	id, ok, err := db.ResolveIdentity(ctx, ident, scene, peer)
	if err != nil || !ok {
		return "", ""
	}
	return id.OpenID, id.UnionID
}

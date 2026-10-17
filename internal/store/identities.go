package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Identity 是某个用户在一个场景下的身份记录。
//
// openid 是平台给出的用户唯一标识（author.id），群聊与单聊对同一个人取同一个值，
// 所以鉴权不依赖这张表也能成立。这张表仍保留两个用途：
//   - 解析主动消息的投递地址（优先用显式记录的单聊 openid）；
//   - 在平台回传 union_openid 时提供跨应用身份的兜底匹配。
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

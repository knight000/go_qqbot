package store

import (
	"context"
	"strings"
	"testing"
)

// 只读不是靠 Go 里判第一个词：`WITH x AS (…) DELETE …` 同样以 with 开头，
// 前缀校验放它过去。真正拦得住的是连接级的 query_only。
func TestQueryRowsRejectsWrites(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cases := []string{
		`DELETE FROM messages`,
		`INSERT INTO messages(event_id, scene, peer_openid, msg_type, created_at) VALUES('x','group','g',0,0)`,
		`WITH x AS (SELECT 1 AS n) DELETE FROM messages WHERE 1 IN (SELECT n FROM x)`,
		`WITH x AS (SELECT 1 AS n) INSERT INTO messages(event_id, scene, peer_openid, msg_type, created_at)
			SELECT 'y','group','g',0,0 FROM x`,
	}
	for _, q := range cases {
		_, err := db.QueryRows(ctx, q)
		if err == nil {
			t.Errorf("写语句应被拒绝: %s", firstLine(q))
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), "readonly") &&
			!strings.Contains(strings.ToLower(err.Error()), "read-only") {
			t.Errorf("错误应说明只读连接，实际: %v", err)
		}
	}

	var n int
	if err := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("写语句不该生效，messages 有 %d 行", n)
	}
}

// 行数上限从"在 SQL 尾巴上拼 LIMIT"改成在取行时兜住：
// 插件自己写了 LIMIT 也不会被拼第二次，没写也不会被读到几十万行。
func TestQueryRowsCapsRowCount(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.QueryRows(context.Background(),
		`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < 2000)
		 SELECT x FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != maxQueryRows {
		t.Fatalf("行数上限 = %d, got %d", maxQueryRows, len(rows))
	}
}

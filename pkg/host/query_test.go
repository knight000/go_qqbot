package host

import (
	"context"
	"testing"
)

type recordingQuery struct {
	got  string
	args []any
}

func (r *recordingQuery) QueryRows(_ context.Context, query string, args ...any) ([]map[string]any, error) {
	r.got, r.args = query, args
	return []map[string]any{{"ok": int64(1)}}, nil
}

// 插件写的 SQL 必须原样到达后端：以前按 `" limit "` 做子串判断，
// `LIMIT\n100` 会被误判成"没写上限"而在尾巴上再拼一个 LIMIT，SQL 直接坏掉。
// 上限改由 store.QueryRows 在取行时兜住。
func TestQueryStoreDoesNotRewriteSQL(t *testing.T) {
	for _, q := range []string{
		"SELECT 1 LIMIT\n100",
		"SELECT 1\nLIMIT 100",
		"SELECT 1 LIMIT 100;",
		"SELECT 1",
	} {
		rec := &recordingQuery{}
		store := &queryStore{db: rec}
		if _, err := store.Query(context.Background(), q); err != nil {
			t.Fatalf("%q 执行失败: %v", q, err)
		}
		if rec.got != q {
			t.Errorf("SQL 被改写:\n 传入 %q\n 实际 %q", q, rec.got)
		}
	}
}

func TestQueryStorePassesArgs(t *testing.T) {
	rec := &recordingQuery{}
	store := &queryStore{db: rec}
	if _, err := store.Query(context.Background(), "SELECT ?", "x"); err != nil {
		t.Fatal(err)
	}
	if len(rec.args) != 1 || rec.args[0] != "x" {
		t.Errorf("参数未透传: %v", rec.args)
	}
}

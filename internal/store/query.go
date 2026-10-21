package store

import (
	"context"
	"database/sql"
	"fmt"
)

// maxQueryRows 是插件只读查询的行数上限。
//
// 上限在取行时兜住，而不是在 SQL 尾巴上拼 LIMIT：拼接既可能拼坏合法 SQL
// （`LIMIT\n100` 认不出来会变成双 LIMIT），也让插件看到的语句与自己写的不一致。
const maxQueryRows = 1000

// QueryRows 执行只读查询并把行转成 map，最多返回 maxQueryRows 行。
//
// 只读由读连接保证（连接级 PRAGMA query_only=ON，见 dsn），因此 `WITH … DELETE`
// 这类写法同样会被拒绝；这里只负责执行与截断。
func (db *DB) QueryRows(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询失败: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("读取列名: %w", err)
	}

	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		if len(out) == maxQueryRows {
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("扫描行: %w", err)
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			switch v := vals[i].(type) {
			case []byte:
				m[c] = string(v)
			case sql.RawBytes:
				m[c] = string(v)
			default:
				m[c] = v
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

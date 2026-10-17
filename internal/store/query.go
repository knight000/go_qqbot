package store

import (
	"context"
	"database/sql"
	"fmt"
)

// QueryRows 执行只读查询并把行转成 map。
// 插件侧（host.QueryStore）已做语句校验与 LIMIT 兜底，这里只负责执行。
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

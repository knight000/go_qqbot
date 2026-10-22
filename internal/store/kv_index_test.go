package store

import (
	"strings"
	"testing"
)

// plugin_kv 的过期清理每 10 分钟跑一次，expires_at 上没有索引就是全表扫。
// 部分索引（只收 expires_at 非空的行）既够用又不会为永不过期的键付代价。
func TestPurgeExpiredKVUsesPartialIndex(t *testing.T) {
	db := openTestDB(t)

	sql, ok := indexSQL(t, db, "idx_plugin_kv_expires")
	if !ok {
		t.Fatal("缺少 expires_at 索引")
	}
	if !strings.Contains(strings.ToUpper(sql), "WHERE EXPIRES_AT IS NOT NULL") {
		t.Errorf("索引应是 expires_at IS NOT NULL 的部分索引，实际: %s", sql)
	}
	plan := explain(t, db, purgeExpiredKVSQL, 0)
	if !strings.Contains(plan, "USING INDEX idx_plugin_kv_expires") {
		t.Errorf("过期 KV 清理应走部分索引，实际计划: %s", plan)
	}
}

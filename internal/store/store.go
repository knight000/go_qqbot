// Package store 是唯一的持久化入口：SQLite（WAL）+ 分批 GC。
//
// 并发模型（ARCHITECTURE.md §4.5）：
//   - 单写连接（MaxOpenConns=1），读连接可多个；
//   - busy_timeout 必须设置，同机 aria2 抢 IO 时这是必要缓冲。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	w    *sql.DB
	r    *sql.DB
	path string
}

func dsn(path string, readOnly bool) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	s := "file:" + filepath.ToSlash(abs) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=auto_vacuum(INCREMENTAL)" +
		"&_pragma=wal_autocheckpoint(1000)"
	if readOnly {
		// 读连接一律 query_only：插件查询的"只读"由 SQLite 自己判定，
		// 而不是靠 Go 里猜 SQL 的第一个词（`WITH … DELETE` 会以 with 开头）。
		// 这条 pragma 是连接级的，连接池里每个连接都会带上。
		s += "&_pragma=query_only(1)"
	}
	return s
}

// Open 打开数据库。目录不存在会自动创建；禁止把 path 指向网络盘或 tmpfs。
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	connect := func(maxOpen int, readOnly bool) (*sql.DB, error) {
		db, err := sql.Open("sqlite", dsn(path, readOnly))
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(maxOpen)
		db.SetMaxIdleConns(maxOpen)
		db.SetConnMaxLifetime(0)
		return db, nil
	}

	w, err := connect(1, false)
	if err != nil {
		return nil, fmt.Errorf("打开写连接: %w", err)
	}
	r, err := connect(4, true)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("打开读连接: %w", err)
	}
	db := &DB{w: w, r: r, path: path}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.w.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}
	return db, nil
}

func (db *DB) Path() string { return db.path }

func (db *DB) Close() error {
	var first error
	if db.w != nil {
		if err := db.w.Close(); err != nil {
			first = err
		}
	}
	if db.r != nil {
		if err := db.r.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// VacuumInto 把当前库完整写成一个独立的快照文件。
//
// 用 SQLite 的 `VACUUM INTO` 而不是拷 .db 文件：库里是 WAL 模式，最近的写入
// 可能还在 -wal 里，只拷主文件会得到一份"少了最后几条消息"的库，而这份快照
// 存在的唯一意义就是回滚时能还原。
//
// 走写连接（MaxOpenConns=1）：VACUUM 期间会独占写，这是有意的——升级路径
// 本来就不该与业务写入并发，而快照的完整性比吞吐重要。
func (db *DB) VacuumInto(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("解析快照路径 %s: %w", path, err)
	}
	// VACUUM INTO 要求目标不存在，自己先判一次能给出更直白的错误。
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("快照目标已存在: %s", abs)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("创建快照目录: %w", err)
	}
	if _, err := db.w.ExecContext(ctx, `VACUUM INTO ?`, filepath.ToSlash(abs)); err != nil {
		return fmt.Errorf("VACUUM INTO %s: %w", abs, err)
	}
	return nil
}

// columnMigrations 给已存在的库补列。SQLite 的 ADD COLUMN 没有 IF NOT EXISTS，
// 所以先查 PRAGMA table_info 再决定是否执行。
var columnMigrations = []struct {
	table  string
	column string
	decl   string
}{
	{"admins", "note", "TEXT NOT NULL DEFAULT ''"},
	{"admins", "union_openid", "TEXT NOT NULL DEFAULT ''"},
	// 旧库没有"退出"这个概念，一律补 0（在群），与实际状态一致。
	{"qq_groups", "left_at", "INTEGER NOT NULL DEFAULT 0"},
	// 旧 reset_audit 只有 actor/peer/level/detail/created_at：
	// 旧行都是"已执行"，所以 status 的默认值取 done 而不是 pending。
	{"reset_audit", "status", "TEXT NOT NULL DEFAULT 'done'"},
	{"reset_audit", "code", "TEXT NOT NULL DEFAULT ''"},
	{"reset_audit", "expires_at", "INTEGER NOT NULL DEFAULT 0"},
	{"reset_audit", "finished_at", "INTEGER NOT NULL DEFAULT 0"},
	// 旧 job_runs 没有会话维度：旧行全部来自全局任务，补 '' 与语义一致。
	{"job_runs", "peer", "TEXT NOT NULL DEFAULT ''"},
}

// Migrate 幂等建表并补齐新增列。
func (db *DB) Migrate(ctx context.Context) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启迁移事务: %w", err)
	}
	for _, stmt := range schema {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("执行 DDL 失败: %w\n%s", err, firstLine(stmt))
		}
	}
	for _, m := range columnMigrations {
		if err := ensureColumn(ctx, tx, m.table, m.column, m.decl); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	// 必须放在补列之后：旧库可能还缺 note / union_openid，先补齐才能完整搬运。
	if err := migrateLegacyAdmins(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	// 同理：plugin_kv 的主键要从 (plugin,k) 换成 (plugin,peer,k)。
	if err := migrateLegacyPluginKV(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	// jobs 的主键要从 id 换成 (id, peer)。必须排在补列之后：
	// job_runs 的 peer 列与本函数末尾的索引都依赖它。
	if err := migrateLegacyJobs(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	// 索引变更排在所有重建之后：重建 plugin_kv 会把它的索引一起带走。
	for _, stmt := range postMigrationStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("执行索引迁移失败: %w\n%s", err, firstLine(stmt))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移: %w", err)
	}
	return nil
}

// migrateLegacyAdmins 把带 peer_scope 的旧 admins 表重建成全局角色表。
//
// 旧模型里授权分场景/分群，新模型里 admin 与 owner 都是全局角色，
// 所以按 openid 去重：owner 优先，否则 admin。整个过程在一个事务里，
// 失败会回滚到旧表，不会留下半成品。
func migrateLegacyAdmins(ctx context.Context, tx *sql.Tx) error {
	old, err := hasColumn(ctx, tx, "admins", "peer_scope")
	if err != nil {
		return err
	}
	if old {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE admins RENAME TO admins_legacy`); err != nil {
			return fmt.Errorf("重命名旧 admins 表: %w", err)
		}
	}
	exists, err := tableExists(ctx, tx, "admins_legacy")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, err := tx.ExecContext(ctx, adminTableDDL); err != nil {
		return fmt.Errorf("重建 admins 表: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO admins(openid, union_openid, role, note, granted_by, source_scene, created_at)
		SELECT openid,
		       COALESCE(MAX(union_openid), ''),
		       CASE WHEN MAX(CASE WHEN role = 'owner' THEN 1 ELSE 0 END) = 1
		            THEN 'owner' ELSE 'admin' END,
		       COALESCE(MAX(note), ''),
		       MAX(granted_by),
		       CASE WHEN MAX(CASE WHEN role = 'owner' THEN 1 ELSE 0 END) = 1
		            THEN MAX(CASE WHEN role = 'owner' THEN scene ELSE '' END)
		            ELSE MAX(scene) END,
		       MIN(created_at)
		  FROM admins_legacy
		 GROUP BY openid`); err != nil {
		return fmt.Errorf("搬运旧管理员授权: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE admins_legacy`); err != nil {
		return fmt.Errorf("删除旧 admins 表: %w", err)
	}
	return nil
}

// migrateLegacyPluginKV 把 plugin_kv 从 (plugin, k) 主键换成 (plugin, peer, k)。
//
// 旧键没有 peer 维度，一律搬进空 peer（插件级全局键）——语义不变：
// 它们本来就是跨会话共享的。R1 只删 peer 非空的键，所以旧数据不会被误清。
//
// SQLite 不支持改主键，只能重建。整个过程一个事务，失败回滚到旧表。
func migrateLegacyPluginKV(ctx context.Context, tx *sql.Tx) error {
	// schema 里的 CREATE TABLE IF NOT EXISTS 已经跑过，表一定存在；
	// 这里仍做一次判断，避免将来有人调整 Migrate 顺序后踩到空表。
	exists, err := tableExists(ctx, tx, "plugin_kv")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	hasPeer, err := hasColumn(ctx, tx, "plugin_kv", "peer")
	if err != nil {
		return err
	}
	if hasPeer {
		return nil // 新库或已迁移
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE plugin_kv RENAME TO plugin_kv_legacy`); err != nil {
		return fmt.Errorf("重命名旧 plugin_kv 表: %w", err)
	}
	if _, err := tx.ExecContext(ctx, kvTableDDL); err != nil {
		return fmt.Errorf("重建 plugin_kv 表: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO plugin_kv(plugin, peer, k, v, expires_at, updated_at)
		SELECT plugin, '', k, v, expires_at, updated_at FROM plugin_kv_legacy`); err != nil {
		return fmt.Errorf("搬运旧插件 KV: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE plugin_kv_legacy`); err != nil {
		return fmt.Errorf("删除旧 plugin_kv 表: %w", err)
	}
	return nil
}

// migrateLegacyJobs 把 jobs 从 id 单主键重建成 (id, peer) 复合主键，
// 并补上 job_runs.peer 与它依赖的索引。
//
// 旧行没有会话维度，一律搬成空 peer（全局任务）——语义不变：
// 它们本来就是一个任务只跑一次、由插件自己决定推给谁。
//
// 重建表会让原表上的索引随旧表一起消失（SQLite 把索引算作表的一部分），
// 所以末尾必须把 idx_jobs_next 重新建一遍，否则扫描会退化成全表扫。
func migrateLegacyJobs(ctx context.Context, tx *sql.Tx) error {
	exists, err := tableExists(ctx, tx, "jobs")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	hasPeer, err := hasColumn(ctx, tx, "jobs", "peer")
	if err != nil {
		return err
	}
	if !hasPeer {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE jobs RENAME TO jobs_legacy`); err != nil {
			return fmt.Errorf("重命名旧 jobs 表: %w", err)
		}
		if _, err := tx.ExecContext(ctx, jobTableDDL); err != nil {
			return fmt.Errorf("重建 jobs 表: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO jobs(id, peer, plugin, spec, timezone, enabled, next_run_at,
			                           last_run_at, last_status, consecutive_failures, payload)
			SELECT id, '', plugin, spec, timezone, enabled, next_run_at,
			       last_run_at, last_status, consecutive_failures, payload
			  FROM jobs_legacy`); err != nil {
			return fmt.Errorf("搬运旧任务: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE jobs_legacy`); err != nil {
			return fmt.Errorf("删除旧 jobs 表: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_jobs_next ON jobs(enabled, next_run_at)`); err != nil {
		return fmt.Errorf("重建任务扫描索引: %w", err)
	}
	// job_runs.peer 由 columnMigrations 补齐，这里只建它上面的索引。
	if _, err := tx.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_job_runs_peer ON job_runs(peer, started_at)`); err != nil {
		return fmt.Errorf("建任务历史 peer 索引: %w", err)
	}
	return nil
}

func tableExists(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
		return false, fmt.Errorf("检查表 %s: %w", table, err)
	}
	return n > 0, nil
}

func hasColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, fmt.Errorf("读取表结构 %s: %w", table, err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var (
			cid       int
			name      string
			typ       string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			return false, fmt.Errorf("解析表结构 %s: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("遍历表结构 %s: %w", table, err)
	}
	return found, nil
}

func ensureColumn(ctx context.Context, tx *sql.Tx, table, column, decl string) error {
	found, err := hasColumn(ctx, tx, table, column)
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+decl); err != nil {
		return fmt.Errorf("为 %s 补列 %s: %w", table, column, err)
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

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

func dsn(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "file:" + filepath.ToSlash(abs) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=auto_vacuum(INCREMENTAL)" +
		"&_pragma=wal_autocheckpoint(1000)"
}

// Open 打开数据库。目录不存在会自动创建；禁止把 path 指向网络盘或 tmpfs。
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	connect := func(maxOpen int) (*sql.DB, error) {
		db, err := sql.Open("sqlite", dsn(path))
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(maxOpen)
		db.SetMaxIdleConns(maxOpen)
		db.SetConnMaxLifetime(0)
		return db, nil
	}

	w, err := connect(1)
	if err != nil {
		return nil, fmt.Errorf("打开写连接: %w", err)
	}
	r, err := connect(4)
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

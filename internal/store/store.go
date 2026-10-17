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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移: %w", err)
	}
	return nil
}

func ensureColumn(ctx context.Context, tx *sql.Tx, table, column, decl string) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return fmt.Errorf("读取表结构 %s: %w", table, err)
	}
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
			_ = rows.Close()
			return fmt.Errorf("解析表结构 %s: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("遍历表结构 %s: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭表结构游标 %s: %w", table, err)
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

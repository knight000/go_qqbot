package store

// schema 与 ARCHITECTURE.md §4.1 完全一致。
// 迁移采用「幂等 DDL 列表」而非版本号：当前是脚手架，表结构变更时
// 追加新的 ALTER 语句并留下注释；正式上线后建议切换到版本化迁移。
var schema = []string{
	`CREATE TABLE IF NOT EXISTS bot_state (
		k          TEXT PRIMARY KEY,
		v          TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS messages (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		event_id      TEXT NOT NULL UNIQUE,
		msg_id        TEXT,
		scene         TEXT NOT NULL,
		peer_openid   TEXT NOT NULL,
		sender_openid TEXT,
		msg_type      INTEGER NOT NULL,
		content       TEXT,
		has_media     INTEGER NOT NULL DEFAULT 0,
		created_at    INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_created ON messages(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_peer    ON messages(peer_openid, created_at)`,

	`CREATE TABLE IF NOT EXISTS processed_events (
		event_id    TEXT PRIMARY KEY,
		received_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_pe_received ON processed_events(received_at)`,

	`CREATE TABLE IF NOT EXISTS media (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id     INTEGER,
		sha256         TEXT NOT NULL,
		kind           TEXT NOT NULL,
		path           TEXT NOT NULL,
		bytes          INTEGER NOT NULL,
		created_at     INTEGER NOT NULL,
		last_access_at INTEGER
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_media_sha     ON media(sha256)`,
	`CREATE INDEX        IF NOT EXISTS idx_media_created ON media(created_at)`,

	`CREATE TABLE IF NOT EXISTS plugin_kv (
		plugin     TEXT NOT NULL,
		k          TEXT NOT NULL,
		v          BLOB NOT NULL,
		expires_at INTEGER,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (plugin, k)
	)`,

	`CREATE TABLE IF NOT EXISTS admins (
		openid       TEXT NOT NULL,
		scene        TEXT NOT NULL,
		peer_scope   TEXT NOT NULL DEFAULT '',
		role         TEXT NOT NULL,
		granted_by   TEXT,
		note         TEXT NOT NULL DEFAULT '',
		union_openid TEXT NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		PRIMARY KEY (openid, scene, peer_scope)
	)`,

	// identities 记录「同一个人在各场景下的身份」。
	// openid 本身跨场景同值（author.id），所以鉴权不依赖这张表；
	// 它用于解析主动消息的投递地址，并在平台回传 union_openid 时提供兜底匹配。
	`CREATE TABLE IF NOT EXISTS identities (
		openid       TEXT NOT NULL,
		scene        TEXT NOT NULL,
		peer_scope   TEXT NOT NULL DEFAULT '',
		union_openid TEXT NOT NULL DEFAULT '',
		last_seen    INTEGER NOT NULL,
		PRIMARY KEY (openid, scene, peer_scope)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_identities_union ON identities(union_openid)`,

	`CREATE TABLE IF NOT EXISTS reset_audit (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		actor      TEXT NOT NULL,
		peer       TEXT,
		level      TEXT NOT NULL,
		detail     TEXT,
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_reset_audit_ts ON reset_audit(created_at)`,

	`CREATE TABLE IF NOT EXISTS jobs (
		id                   TEXT PRIMARY KEY,
		plugin               TEXT NOT NULL,
		spec                 TEXT NOT NULL,
		timezone             TEXT NOT NULL DEFAULT 'Asia/Shanghai',
		enabled              INTEGER NOT NULL DEFAULT 1,
		next_run_at          INTEGER NOT NULL,
		last_run_at          INTEGER,
		last_status          TEXT,
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		payload              TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_jobs_next ON jobs(enabled, next_run_at)`,

	`CREATE TABLE IF NOT EXISTS job_runs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id      TEXT NOT NULL,
		started_at  INTEGER NOT NULL,
		finished_at INTEGER,
		status      TEXT,
		error       TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_job_runs ON job_runs(job_id, started_at)`,
}

package store

// adminTableDDL 单独提出来，因为旧库迁移要先把旧表改名、再用它重建。
//
// 角色是**全局**的：openid 跨场景同值，群内添加与私聊添加效果一致，
// 因此主键只有 openid。source_scene 只记录"在哪类会话里被添加"，不参与判定。
const adminTableDDL = `CREATE TABLE IF NOT EXISTS admins (
		openid       TEXT PRIMARY KEY,
		union_openid TEXT NOT NULL DEFAULT '',
		role         TEXT NOT NULL,
		note         TEXT NOT NULL DEFAULT '',
		granted_by   TEXT,
		source_scene TEXT NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL
	)`

// kvTableDDL 同样单独提出来：R1 需要"按 peer 清会话"，
// 所以主键必须是 (plugin, peer, k)，旧库升级时要重建表。
//
// peer 空串 = 插件级全局键（跨会话共享，R1 不动它）；
// peer 非空 = 该会话私有的键（R1 只删这些）。
const kvTableDDL = `CREATE TABLE IF NOT EXISTS plugin_kv (
		plugin     TEXT NOT NULL,
		peer       TEXT NOT NULL DEFAULT '',
		k          TEXT NOT NULL,
		v          BLOB NOT NULL,
		expires_at INTEGER,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (plugin, peer, k)
	)`

// activeUsageTableDDL 单独提出来只是为了和 kvTableDDL 一样读得清楚：
// 它是"核心自己的账"，与插件 KV 完全不共享命名空间。
const activeUsageTableDDL = `CREATE TABLE IF NOT EXISTS active_usage (
		day   TEXT NOT NULL,
		scene TEXT NOT NULL,
		peer  TEXT NOT NULL,
		used  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (day, scene, peer)
	)`

// sessionTableDDL 单独提出来只为读得清楚：它是"隐式路由"的唯一事实源，
// 主键选择本身就是 ADR-0004 决策 1 的落地。
//
// 主键只有 peer：同 peer 同时只允许一个持有者。scene 是冗余列，只用于观测
// 与消费校验——把它放进主键会得到 (scene, peer) 两个持有者，直接推翻决策 1。
const sessionTableDDL = `CREATE TABLE IF NOT EXISTS sessions (
		peer       TEXT PRIMARY KEY,
		scene      TEXT NOT NULL DEFAULT '',
		plugin     TEXT NOT NULL,
		state      BLOB NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	)`

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

	kvTableDDL,

	// active_usage 是主动消息的本地日预算（§3.3）。核心不能写 plugin_kv
	// ——那是插件命名空间，插件一清前缀就把额度账抹了。
	//
	// 没有内存镜像：发送前用一条 upsert 原子地"检查 + 自增"，于是
	// 重启不清零、并发与多进程都不会超发。day 取调度时区（Asia/Shanghai，
	// 与 cron 同源，由核心注入）的 YYYY-MM-DD，跨日就是新主键，
	// 换桶不需要任何重置动作。
	activeUsageTableDDL,

	adminTableDDL,

	// qq_groups 是"见过的群"的注册表，也是黑白名单的唯一存储。
	//
	// 平台没有"列出机器人加入的群"的接口，所以只能靠收到群事件时登记；
	// group_name 可能为空（群信息接口 11253 需要内邀），由 /group name 手工补。
	// mode: ''=未设置 / 'allow'=白名单 / 'deny'=黑名单。
	// left_at: 0=机器人仍在群里；>0=退出时间。退群不删记录——
	// 删了就连"这个群曾经存在过"都查不到，而名单与历史活跃时间还有诊断价值。
	`CREATE TABLE IF NOT EXISTS qq_groups (
		group_openid  TEXT PRIMARY KEY,
		group_name    TEXT NOT NULL DEFAULT '',
		name_source   TEXT NOT NULL DEFAULT '',
		mode          TEXT NOT NULL DEFAULT '',
		left_at       INTEGER NOT NULL DEFAULT 0,
		first_seen_at INTEGER NOT NULL,
		last_seen_at  INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_qq_groups_mode ON qq_groups(mode, last_seen_at)`,

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

	// reset_audit 记录每一次重置尝试（§8.3）。不可逆操作必须能事后回答
	// "谁在什么时候按的、按到了哪一级"。
	//
	// status: pending（等确认码）/ done（成功）/ failed（执行失败）/ cancelled（被新的同目标请求顶掉）/ expired。
	// done 与 failed 的区别是冷却：只有 done 参与 LastResetDoneAt，失败的尝试不占窗口。
	// 二次确认走这张表而不是内存 map：进程重启后 pending 仍可核对，
	// 也不会出现"审计里只有 done、看不到曾有人尝试过"的盲区。
	// expires_at 只对 pending 有意义（确认码 60 秒）。
	`CREATE TABLE IF NOT EXISTS reset_audit (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		actor       TEXT NOT NULL,
		peer        TEXT NOT NULL DEFAULT '',
		level       TEXT NOT NULL,
		status      TEXT NOT NULL DEFAULT 'done',
		code        TEXT NOT NULL DEFAULT '',
		detail      TEXT,
		created_at  INTEGER NOT NULL,
		expires_at  INTEGER NOT NULL DEFAULT 0,
		finished_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_reset_audit_ts ON reset_audit(created_at)`,

	jobTableDDL,
	`CREATE INDEX IF NOT EXISTS idx_jobs_next ON jobs(enabled, next_run_at)`,

	// job_runs 带 peer，R2 才清得掉「某个群的执行历史」。
	// 这个列不加就只能在 jobs 上做 join，而 jobs 的 per-peer 行将来可能被清理。
	//
	// 注意：peer 列由 jobRunsTableDDL 保证，已在旧库上补列；
	// idx_job_runs_peer 不能写在这里——schema 先于补列执行，
	// 旧库此时还没有 peer 列，建索引会直接失败（reset_audit 踩过同一个坑）。
	jobRunsTableDDL,

	// sessions 是新表，旧库升级不需要迁移：幂等 DDL 直接把它建出来。
	// 两个索引各有明确用途——expires 给 GC 的 DELETE 走索引，
	// plugin 只服务排障查询（"重载后留下了哪些行"），不在热路径。
	sessionTableDDL,
	`CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_plugin  ON sessions(plugin)`,
}

// postMigrationStatements 是**必须排在建表与补列之后**的索引变更。
//
// 放在这里而不是 schema 列表里，有两个原因：
//   - migrateLegacyPluginKV 会重建 plugin_kv，重建会把表上的索引一起带走，
//     schema 里建好的索引会在迁移末尾消失；
//   - job_runs 的旧索引要先 DROP 再建新的，且新索引列必须已经存在。
//
// 每条都用 IF [NOT] EXISTS，所以重复迁移是幂等的。
var postMigrationStatements = []string{
	// GC 按 started_at 分批删（PurgeJobRuns），唯一的消费方就是这条查询。
	// 旧的 (job_id, started_at) 没有任何查询用得上，只增加写放大。
	`DROP INDEX IF EXISTS idx_job_runs`,
	`CREATE INDEX IF NOT EXISTS idx_job_runs_started ON job_runs(started_at)`,
}

// jobTableDDL 单独提出来：旧库要把 jobs 从 id 主键重建成 (id, peer) 复合主键。
//
// peer 是任务的会话维度：
//   - 空串 = 全局任务，只跑一次（插件自己决定推给谁）；
//   - 非空 = 该会话的实例，RunJob 时 Host 把 Peer 填成它。
//
// per-peer 实例的启停**不额外记状态**：DueJobs 会 join qq_groups 要求
// left_at = 0。退群即停、入群即恢复由群登记表这一个事实源表达，
// 而不是再放一份会与熔断、未注册等停用原因互相覆盖的开关。
const jobTableDDL = `CREATE TABLE IF NOT EXISTS jobs (
		id                   TEXT NOT NULL,
		peer                 TEXT NOT NULL DEFAULT '',
		plugin               TEXT NOT NULL,
		spec                 TEXT NOT NULL,
		timezone             TEXT NOT NULL DEFAULT 'Asia/Shanghai',
		enabled              INTEGER NOT NULL DEFAULT 1,
		next_run_at          INTEGER NOT NULL,
		last_run_at          INTEGER,
		last_status          TEXT,
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		payload              TEXT,
		PRIMARY KEY (id, peer)
	)`

const jobRunsTableDDL = `CREATE TABLE IF NOT EXISTS job_runs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id      TEXT NOT NULL,
		peer        TEXT NOT NULL DEFAULT '',
		started_at  INTEGER NOT NULL,
		finished_at INTEGER,
		status      TEXT,
		error       TEXT
	)`

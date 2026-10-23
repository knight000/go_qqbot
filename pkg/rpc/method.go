package rpc

// 方法表。名字是线协议的一部分：改一个名字等于改协议，等于要所有插件一起升级
// ——所以它们写成常量而不是散落在调用点的字符串字面量。
//
// 命名口径 `<侧>.<能力>`，与 pkg/host 的接口一一对应：
//
//	host.Plugin  Manifest / Init / Handle / (Start) / (Close) / (RunJob)
//	             → plugin.manifest / plugin.init / plugin.handle
//	               / plugin.start / plugin.close / plugin.run_job
//	host.Host    Sender / KV / PeerKV / Query / HTTP / Scheduler /
//	             IsAdmin / Admins / Renderer / Session
//	             → host.reply / host.send_active
//	               host.kv_get / host.kv_set / host.kv_delete
//	               host.query
//	               host.http_get / host.http_post / host.http_get_with_header
//	               / host.http_get_once
//	               host.schedule / host.is_admin / host.admin_targets
//	               host.render / host.session_await
//
// 三个访问器**不过通道**，因为它们只是把握手时已经给出的值原样返回：
//
//	Host.Plugin()  ← plugin.init 的 name
//	Host.Config()  ← plugin.init 的 config（不可变快照，ADR-0005「本 ADR 不做」
//	                 一节明写插件配置不热更新）
//	Host.Log()     ← 插件自己的 stderr（决策 4：日志不过 RPC，由核心抓 stderr）
//
// 这不是遗漏。给它们各开一个方法意味着每次打日志、每个 handler 里的
// `h.Plugin()` 都要一次往返，而它们返回的是同一个常量；更重要的是它会
// 把「读配置」变成一次可能失败、可能超时的 IPC——那类调用点通常没有
// 错误处理的位置（`if h.Config().Str(...)` 写不成三行）。
const (
	// plugin.* 由插件侧实现，核心调用。
	MethodPluginManifest = "plugin.manifest"
	MethodPluginInit     = "plugin.init"
	MethodPluginHandle   = "plugin.handle"
	MethodPluginStart    = "plugin.start"
	MethodPluginClose    = "plugin.close"
	MethodPluginRunJob   = "plugin.run_job"

	// host.* 由核心实现，插件调用。
	MethodHostReply         = "host.reply"
	MethodHostSendActive    = "host.send_active"
	MethodHostKVGet         = "host.kv_get"
	MethodHostKVSet         = "host.kv_set"
	MethodHostKVDelete      = "host.kv_delete"
	MethodHostQuery         = "host.query"
	MethodHostHTTPGet       = "host.http_get"
	MethodHostHTTPPost      = "host.http_post"
	MethodHostHTTPGetHeader = "host.http_get_with_header"
	MethodHostHTTPGetOnce   = "host.http_get_once"
	MethodHostSchedule      = "host.schedule"
	MethodHostIsAdmin       = "host.is_admin"
	MethodHostAdminTargets  = "host.admin_targets"
	MethodHostRender        = "host.render"
	MethodHostSessionAwait  = "host.session_await"
)

// OptionalPluginMethods 是 Plugin 的三个可选接口对应的方法名。
//
// host.Plugin 只要求 Manifest / Init / Handle；Starter / Closer / JobRunner
// 是类型断言出来的。跨进程没有类型可断言，所以由 plugin.manifest 的应答
// 把「本产物到底实现了哪几个」报给核心（Serve 里那次类型断言的结果）。
// 核心据此决定要不要发 plugin.start，而不是靠「调用失败再当没有」。
const (
	OptionalStart  = MethodPluginStart
	OptionalClose  = MethodPluginClose
	OptionalRunJob = MethodPluginRunJob
)

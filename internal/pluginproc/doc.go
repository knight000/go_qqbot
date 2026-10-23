// Package pluginproc 把「一个可执行文件」变成核心眼里的 host.Plugin（ADR-0005）。
//
// 它是 P3 唯一的进程边界：核心起 `<datadir>/plugins/bin/<name>`，两侧走
// pkg/rpc 的长度前缀帧，插件侧由 pkg/rpcsdk 应答。核心的路由、权限、会话、
// 调度、渲染都不认识「进程」这个概念——它们只认 host.Plugin，而这个包提供的
// proxy 就是那个接口的一份实现（Manifest 是装配期缓存值，其余转发 RPC）。
//
// 三件事在这个包里必须成立，否则整条方案不成立：
//
//   - **启动不因单个插件而失败**。探测失败、超时、api_version 不符一律跳过
//     并记 Error（ADR-0005 决策 6）：配置写错是人改一下文件的事，插件产物
//     过期是一条网络命令的事；为一个过期插件让机器人整体下线，还会进
//     systemd 重启循环白烧 Identify 配额。
//   - **超时不因跨进程而放宽**。Handle 3 s、Job 60 s（rpc.HandleTimeout /
//     rpc.JobTimeout 是唯一口径），超时即取消该次调用并计入熔断计数
//     （决策 7）。
//   - **崩溃不带走核心**。子进程非零退出按 1/2/4/8/16 分钟退避重启，连败
//     5 次停用该插件并留一条可读的「已停用」（决策 8）。关闭走
//     RPC close → SIGTERM → Kill 三级，每级带超时，不留孤儿进程。
package pluginproc

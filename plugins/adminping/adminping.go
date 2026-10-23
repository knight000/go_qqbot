// Package adminping 提供仅管理员可用的健康检查：/ping 返回运行时长与资源占用。
//
// 鉴权是**声明式**的：Manifest 里写 Permission = PermBotAdmin，核心在路由阶段
// 就拦下非管理员，本插件不出现任何判权语句。这比每个插件自己查 IsAdmin 更安全：
// 插件漏写检查不会导致越权，拒绝语也由核心统一发出。
//
// 安全边界：
//   - 只有管理员能拿到任何指标；
//   - 拒绝语由核心控制（[permission].deny_reply，默认关闭 = 完全静默），
//     避免在群里暴露该指令的存在与机器信息；
//   - 只看当前进程与所在主机的聚合指标，不读环境变量、不碰密钥。
//
// 跨平台：指标采集全部在 internal/sysinfo 里按 build tag 分实现，
// 本文件不含任何平台相关代码。
package adminping

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/buildinfo"
	"github.com/knight000/go_qqbot/internal/sysinfo"
	"github.com/knight000/go_qqbot/pkg/host"
)

// readProcessMemory 是本进程内存的采集入口。做成变量只为一件事：支持平台上
// 真实采集几乎不会失败，"采集失败"分支只能靠注入来验证。
var readProcessMemory = sysinfo.ReadProcessMemory

type Plugin struct {
	log *slog.Logger
}

func (p *Plugin) Manifest() host.Manifest {
	return host.Manifest{
		Name:         "adminping",
		Version:      "0.2.0",
		APIVersion:   host.APIVersion,
		Capabilities: []string{"sender.reply", "log"},
		Intents:      []string{"message"},
		Commands: []host.Command{{
			Prefix:      "/ping",
			Priority:    200,
			Description: "仅管理员：运行状态与资源占用",
			Permission:  host.PermBotAdmin,
			DenyReply:   "该指令仅管理员可用。",
		}},
	}
}

func (p *Plugin) Init(ctx context.Context, h host.Host) error {
	p.log = h.Log()
	p.log.Info("adminping 插件已初始化", "权限", host.PermBotAdmin.String())
	return nil
}

// Handle 只认路由层填好的 Event.Command：`/ping` 命中的事件才投递过来，
// 匹配规则（含大小写敏感、词边界）由 core.argsAfterPrefix 统一决定。
// 插件不再按原文再判一次——那会让"这是不是一条指令"出现第二个仲裁者，
// 也会把路由层刻意排除的大小写变体（`/PING`）放进来。
func (p *Plugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	if ev.Command != "/ping" {
		return nil, nil
	}

	text, err := p.report(ctx)
	if err != nil {
		// 走到这里说明连 os.Getwd 之类的基础能力都失败了
		p.log.Warn("生成 /ping 报告失败", "err", err)
		return &host.Reply{Content: "/pong（指标采集失败：" + err.Error() + "）"}, nil
	}
	return &host.Reply{Content: text}, nil
}

func (p *Plugin) report(ctx context.Context) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("取当前目录: %w", err)
	}
	// 解析软链接，展示真实路径；失败则保留 wd
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}

	var b strings.Builder
	b.WriteString("/pong\n")

	// 0. 版本：重启/升级后用它确认跑的是不是新二进制（+dirty 表示工作区有未提交改动）
	bi := buildinfo.Get()
	fmt.Fprintf(&b, "版本 %s｜%s\n", bi.String(), strings.TrimPrefix(bi.GoVer, "go"))

	// 1. 连续运行时长
	up := time.Since(sysinfo.StartTime)
	fmt.Fprintf(&b, "运行 %s（启动于 %s）｜%s/%s\n",
		sysinfo.HumanDuration(up),
		sysinfo.StartTime.Format("01-02 15:04"),
		runtime.GOOS, runtime.GOARCH)

	// 2. CPU
	cpu, cpuErr := sysinfo.ReadCPU(ctx)
	if cpuErr != nil {
		fmt.Fprintf(&b, "CPU  采集失败（%v）\n", cpuErr)
	} else {
		fmt.Fprintf(&b, "CPU  %.1f%%（%d 核）\n", cpu.Percent, cpu.Cores)
	}

	// 3. 内存：整机一行、本进程一行。两者是独立数据源，整机取不到不该把
	// 本进程也抹掉；本进程取不到要显式说"采集失败"，而不是让整行消失。
	mem, memErr := sysinfo.ReadMemory()
	if memErr != nil {
		fmt.Fprintf(&b, "内存 采集失败（%v）\n", memErr)
	} else {
		fmt.Fprintf(&b, "内存 %s / %s（%.1f%%）\n",
			sysinfo.HumanBytes(int64(mem.Used)),
			sysinfo.HumanBytes(int64(mem.Total)),
			mem.Percent)
	}
	if pm, err := readProcessMemory(); err != nil {
		fmt.Fprintf(&b, "本进程 采集失败（%v）\n", err)
	} else {
		fmt.Fprintf(&b, "本进程 %s（峰值 %s）\n",
			sysinfo.HumanBytes(int64(pm.RSS)),
			sysinfo.HumanBytes(int64(pm.Peak)))
	}

	// 4. 分区余量（当前目录所在分区）
	disk, diskErr := sysinfo.ReadDisk(wd)
	if diskErr != nil {
		fmt.Fprintf(&b, "磁盘 采集失败（%v）\n", diskErr)
	} else {
		fmt.Fprintf(&b, "磁盘 可用 %s / 共 %s（已用 %.1f%%）\n",
			sysinfo.HumanBytes(int64(disk.Free)),
			sysinfo.HumanBytes(int64(disk.Total)),
			100*float64(disk.Used)/float64(disk.Total))
	}

	// 5. 机器人目录占用
	usage, dirErr := sysinfo.DirSize(ctx, wd)
	if dirErr != nil {
		fmt.Fprintf(&b, "目录 %s 统计失败（%v）\n", wd, dirErr)
	} else {
		fmt.Fprintf(&b, "目录 %s %s，%d 个文件",
			usage.Path, sysinfo.HumanBytes(usage.Bytes), usage.Files)
		if usage.IgnoredGit {
			b.WriteString("（不含 .git）")
		}
		if usage.Partial {
			b.WriteString("（未走完，结果为下限）")
		}
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n"), nil
}

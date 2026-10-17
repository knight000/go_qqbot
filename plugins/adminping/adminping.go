// Package adminping 提供仅管理员可用的健康检查：/ping 返回运行时长与资源占用。
//
// 安全边界：
//   - 只有管理员能拿到任何指标；非管理员默认完全静默（[plugins.adminping] silent = true），
//     连"无权限"都不回，避免在群里暴露该指令的存在与机器信息；
//   - 鉴权失败按 fail-closed 处理，与"不是管理员"同等对待；
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

	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/sysinfo"
)

func init() { host.Register(&Plugin{}) }

type Plugin struct {
	log  *slog.Logger
	host host.Host
	// silent 控制非管理员（及鉴权失败）时是否完全静默。
	// 默认 true：连"无权限"都不回，避免在群里暴露该指令与机器信息。
	silent bool
}

func (p *Plugin) Manifest() host.Manifest {
	return host.Manifest{
		Name:         "adminping",
		Version:      "0.1.0",
		APIVersion:   host.APIVersion,
		Capabilities: []string{"sender.reply", "admin.check", "log"},
		Intents: []string{
			host.EventGroupMessage,
			host.EventC2CMessage,
		},
		Commands: []host.Command{
			{Prefix: "/ping", Priority: 200, Description: "仅管理员：运行状态与资源占用"},
		},
		ConfigSchema: map[string]string{
			"silent": "bool",
		},
	}
}

func (p *Plugin) Init(ctx context.Context, h host.Host) error {
	p.host = h
	p.log = h.Log()
	p.silent = h.Config().Bool("silent", true)
	p.log.Info("adminping 插件已初始化", "仅管理员", true, "非管理员静默", p.silent)
	return nil
}

func (p *Plugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	if !isPing(ev.Content) {
		return nil, nil
	}

	ok, err := p.host.IsAdmin(ctx, ev.Identity())
	if err != nil {
		// 权限查询失败一律按"不是管理员"处理：宁可管理员看不到数据，
		// 也不能因为后端故障把机器信息回给陌生人。
		p.log.Warn("查询管理员身份失败，忽略 /ping",
			"scene", ev.Scene, "peer", ev.PeerID, "err", err)
		return p.deny("权限查询失败，本次请求已忽略。"), nil
	}
	if !ok {
		p.log.Debug("非管理员请求 /ping，已忽略", "scene", ev.Scene, "peer", ev.PeerID)
		return p.deny("该指令仅管理员可用。"), nil
	}

	text, err := p.report(ctx)
	if err != nil {
		// 走到这里说明连 os.Getwd 之类的基础能力都失败了
		p.log.Warn("生成 /ping 报告失败", "err", err)
		return &host.Reply{Content: "/pong（指标采集失败：" + err.Error() + "）"}, nil
	}
	return &host.Reply{Content: text}, nil
}

// isPing 只认精确的 /ping，避免 /pingxxx 被误判。
func isPing(content string) bool {
	fields := strings.Fields(strings.TrimSpace(content))
	return len(fields) > 0 && strings.EqualFold(fields[0], "/ping")
}

// deny 在非静默模式下回一句拒绝；静默模式（默认）返回 nil，什么都不发。
func (p *Plugin) deny(msg string) *host.Reply {
	if p.silent {
		return nil
	}
	return &host.Reply{Content: msg}
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

	// 3. 内存
	mem, memErr := sysinfo.ReadMemory()
	if memErr != nil {
		fmt.Fprintf(&b, "内存 采集失败（%v）\n", memErr)
	} else {
		fmt.Fprintf(&b, "内存 %s / %s（%.1f%%）",
			sysinfo.HumanBytes(int64(mem.Used)),
			sysinfo.HumanBytes(int64(mem.Total)),
			mem.Percent)
		if mem.ProcessRSS > 0 {
			fmt.Fprintf(&b, "｜本进程 %s", sysinfo.HumanBytes(int64(mem.ProcessRSS)))
		}
		b.WriteString("\n")
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

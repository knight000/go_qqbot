package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/buildinfo"
	"github.com/knight000/go_qqbot/pkg/host"
)

// /reload 是"把磁盘上的新二进制换上来"的唯一入口。
//
// 为什么必须由核心独占：它是进程级操作，插件一旦能触发就等于能随时让机器人下线。
// 为什么不是热重载：Go 的插件是编译期注册的静态链接代码，运行中无法替换；
// 真正能验证"更新是否生效"的只有重新执行自身。
//
// 语义（与 /reset 无关，重置数据属于另一条线）：
//   - 先回一条确认消息（被动回复窗口只在收到消息时存在，重启后就发不了了）；
//   - 回复送达后由 main 关闭网关、落盘 session、关闭数据库；
//   - Unix 用 exec 原地替换进程映像（PID 不变，systemd cgroup 不受影响）；
//     Windows 用 spawn 拉起自身后退出（没有 systemd，不存在 cgroup 回收问题）。

// RestartRequest 由 /reload 产生，交给 main 执行重启动作。
type RestartRequest struct {
	By     string     // 触发者 openid
	Role   string     // owner / admin
	Scene  host.Scene // 触发场景
	Reason string     // 可选备注，写进日志
	At     time.Time
}

// ErrRestartPending 表示已经有一条重启请求在队列里，避免重复触发。
var ErrRestartPending = errors.New("已有重启请求在处理")

const reloadUsage = `用法：
/reload [说明]   重启进程，加载磁盘上的新二进制（管理员）
/restart [说明]   同上

重启前会先回这条消息，随后进程退出并重新执行自己。
约 1 分钟内会收到私聊「机器人已上线」，用 /ping 对比版本与启动时间即可确认更新生效。
说明  : 会写进日志，方便回溯谁在什么时候重启过。`

// RestartRequests 暴露给 main：收到值即表示需要重启。
func (c *Core) RestartRequests() <-chan RestartRequest { return c.restart }

func (c *Core) requestRestart(req RestartRequest) error {
	select {
	case c.restart <- req:
		return nil
	default:
		return ErrRestartPending
	}
}

func (c *Core) handleReload(ctx context.Context, ev *host.Event) {
	p := c.authorize(ctx, ev)
	if !p.IsBotAdmin() {
		// 与 adminping 的静默策略不同：/reload 属于核心指令，
		// 明确回一句比让发送者以为"没反应"更好，也不泄露任何机器信息。
		c.replyText(ctx, ev, "该指令仅管理员可用。")
		return
	}

	fields := strings.Fields(strings.TrimSpace(ev.Content))
	args := fields[1:]
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "help", "-h", "--help":
			c.replyText(ctx, ev, reloadUsage)
			return
		}
	}
	reason := strings.TrimSpace(strings.Join(args, " "))
	if r := []rune(reason); len(r) > 60 {
		reason = string(r[:60])
	}

	// 托管模式下二进制路径由 devrun 掌握，本进程定位失败也不影响重载。
	exe, exeErr := os.Executable()
	if exeErr != nil && !buildinfo.Supervised() {
		c.log.Error("定位可执行文件失败，取消重启", "err", exeErr)
		c.replyText(ctx, ev, "重启失败：定位不到可执行文件，请查看日志。")
		return
	}

	bi := buildinfo.Get()
	var b strings.Builder
	b.WriteString("正在重启\n")
	fmt.Fprintf(&b, "版本 %s｜%s\n", bi.String(), bi.Platform)
	fmt.Fprintf(&b, "已运行 %s（启动于 %s）\n",
		humanUptime(time.Since(c.startedAt)),
		c.startedAt.Format("01-02 15:04:05"))
	if reason != "" {
		fmt.Fprintf(&b, "备注 %s\n", reason)
	}
	if buildinfo.Supervised() {
		b.WriteString("模式 受 devrun 托管：会重新扫描 plugins/ 并编译，失败则沿用旧二进制\n")
	} else if isGoRunBinary(exe) {
		b.WriteString("注意 当前由 go run 运行：重启的是同一份临时构建，不会重新编译\n")
	}
	b.WriteString("重启后会用私聊发「机器人已上线」，也可用 /ping 对比版本与启动时间。")

	// 先发回复再入队：被动回复窗口在重启后就不存在了，顺序反了这条消息必丢。
	c.replyText(ctx, ev, b.String())

	req := RestartRequest{By: ev.SenderID, Role: string(p.Role), Scene: ev.Scene, Reason: reason, At: time.Now()}
	if err := c.requestRestart(req); err != nil {
		c.log.Warn("重启请求未入队", "by", ev.SenderID, "err", err)
		return
	}
	c.log.Warn("收到 /reload，准备重启",
		"by", ev.SenderID, "role", p.Role, "scene", ev.Scene, "reason", reason, "exe", exe)
}

// isGoRunBinary 判断当前二进制是不是 go run 的临时产物。
// 这种情况下 exec 只是再跑一遍同一份临时文件，源码改动不会生效，必须提示。
func isGoRunBinary(exe string) bool {
	return strings.Contains(filepath.ToSlash(exe), "/go-build")
}

// humanUptime 用两级精度描述运行时长：刚启动按秒，久了按小时/天。
func humanUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d分", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%d小时", h)
		}
		return fmt.Sprintf("%d小时%d分", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) - days*24
	if h == 0 {
		return fmt.Sprintf("%d天", days)
	}
	return fmt.Sprintf("%d天%d小时", days, h)
}

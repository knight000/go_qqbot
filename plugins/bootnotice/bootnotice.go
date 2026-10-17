// Package bootnotice 在进程启动（含重启）后主动私聊管理员，
// 报告上线时间、距上次启动的间隔，以及当前启用的插件清单。
//
// 为什么用主动消息：启动时没有任何用户消息可回复，被动回复窗口不存在，
// 只能走主动单聊。平台对主动消息有每日额度，因此这里做了三层收敛：
//   - 只有 [active].plugins 白名单里的插件能发（本插件默认在名单内）；
//   - 同一进程只在启动后发一次，失败最多重试一次；
//   - 距上次启动不足 min_interval 时整轮跳过，避免崩溃重启刷屏。
//
// 收件人来自 Host.Admins()：openid 跨场景同值，群里的管理员也能直接私聊；
// 若某条授权连 openid 都解析不出来，跳过并在日志里说明。
package bootnotice

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"github.com/knight000/qqbot/internal/buildinfo"
	"github.com/knight000/qqbot/internal/host"
	"github.com/knight000/qqbot/internal/sysinfo"
)

func init() { host.Register(&Plugin{}) }

type Plugin struct {
	host host.Host
	log  *slog.Logger

	enabled     bool
	delay       time.Duration
	minInterval time.Duration
	retryDelay  time.Duration
}

func (p *Plugin) Manifest() host.Manifest {
	return host.Manifest{
		Name:         "bootnotice",
		Version:      "0.1.0",
		APIVersion:   host.APIVersion,
		Capabilities: []string{"sender.active", "admin.directory", "kv", "log"},
		// 不发消息事件：本插件只在启动时动作，不参与路由匹配。
		ConfigSchema: map[string]string{
			"enabled":      "bool",
			"delay":        "duration",
			"min_interval": "duration",
			"retry_delay":  "duration",
		},
	}
}

func (p *Plugin) Init(ctx context.Context, h host.Host) error {
	p.host = h
	p.log = h.Log()
	cfg := h.Config()
	p.enabled = cfg.Bool("enabled", true)
	p.delay = cfg.Duration("delay", 10*time.Second)
	p.minInterval = cfg.Duration("min_interval", 10*time.Minute)
	p.retryDelay = cfg.Duration("retry_delay", 30*time.Second)
	p.log.Info("bootnotice 插件已初始化",
		"enabled", p.enabled, "delay", p.delay.String(), "min_interval", p.minInterval.String())
	return nil
}

// Handle 不会被调用：Manifest 没有订阅任何事件类型，路由器不会命中本插件。
func (p *Plugin) Handle(context.Context, *host.Event) (*host.Reply, error) { return nil, nil }

// Start 只登记协程，不阻塞核心启动：网关还没连上时也应该能发主动消息，
// 但等一小会儿可以让 token 与插件初始化都就绪。
func (p *Plugin) Start(ctx context.Context) error {
	if !p.enabled {
		return nil
	}
	go p.run(ctx)
	return nil
}

func (p *Plugin) run(ctx context.Context) {
	if !sleepCtx(ctx, p.delay) {
		return
	}

	prev, hasPrev := p.readStart(ctx)
	// 无论这轮发不发，都记下本次启动：这样崩溃循环里的每次重启都会
	// 把 min_interval 的窗口往后推，而不是反复触发通知。
	p.writeStart(ctx, sysinfo.StartTime)

	targets, err := p.host.Admins().Targets(ctx)
	if err != nil {
		p.log.Warn("读取管理员列表失败，跳过启动通知", "err", err)
		return
	}
	var reachable []host.AdminTarget
	for _, t := range targets {
		if t.C2COpenID != "" {
			reachable = append(reachable, t)
		}
	}
	if len(reachable) == 0 {
		p.log.Info("没有可私聊的管理员，跳过启动通知",
			"admins", len(targets),
			"原因", "管理员列表为空，或授权里没有可用的 openid")
		return
	}
	// /reload 是管理员显式要求重启，此时必须发——否则"重启后确认更新是否生效"
	// 会被崩溃抑制窗口吞掉，变成一次没有回执的操作。
	if hasPrev && p.minInterval > 0 && time.Since(prev) < p.minInterval && !buildinfo.RestartedByReload() {
		p.log.Info("距上次启动不足 min_interval，跳过通知",
			"last", prev.Format(time.RFC3339), "min_interval", p.minInterval.String())
		return
	}

	text := compose(sysinfo.StartTime, prev, hasPrev)
	sent := 0
	for _, t := range reachable {
		if err := p.send(ctx, t.C2COpenID, text); err != nil {
			p.log.Warn("启动通知发送失败",
				"role", t.Role, "peer", t.C2COpenID, "err", err)
			continue
		}
		sent++
	}
	p.log.Info("启动通知已发送", "sent", sent, "total", len(reachable))
}

func (p *Plugin) send(ctx context.Context, openid, text string) error {
	msg := host.Reply{Content: text}
	err := p.host.Sender().SendActive(ctx, host.SceneC2C, openid, msg)
	if err == nil || p.retryDelay <= 0 {
		return err
	}
	// 只重试一次：超时可能已经送达，主动消息没有 msg_id 幂等键，多试就是重复推送。
	if !sleepCtx(ctx, p.retryDelay) {
		return err
	}
	return p.host.Sender().SendActive(ctx, host.SceneC2C, openid, msg)
}

func (p *Plugin) readStart(ctx context.Context) (time.Time, bool) {
	raw, ok, err := p.host.KV().Get(ctx, keyLastStart)
	if err != nil || !ok {
		if err != nil {
			p.log.Debug("读取上次启动时间失败", "err", err)
		}
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, string(raw))
	if err != nil {
		p.log.Debug("上次启动时间格式异常", "value", string(raw), "err", err)
		return time.Time{}, false
	}
	return t, true
}

func (p *Plugin) writeStart(ctx context.Context, t time.Time) {
	if err := p.host.KV().Set(ctx, keyLastStart, []byte(t.Format(time.RFC3339)), 0); err != nil {
		p.log.Warn("记录启动时间失败", "err", err)
	}
}

const keyLastStart = "last_start"

// compose 生成通知正文。时间用本地时区（进程启动时已嵌入 tzdata）。
func compose(start, prev time.Time, hasPrev bool) string {
	var b strings.Builder
	b.WriteString("机器人已上线\n")
	bi := buildinfo.Get()
	fmt.Fprintf(&b, "版本 %s", bi.String())
	if buildinfo.RestartedByReload() {
		b.WriteString("（/reload 重启）")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "启动 %s｜%s/%s\n", start.Format("01-02 15:04:05"), runtime.GOOS, runtime.GOARCH)
	if hasPrev {
		fmt.Fprintf(&b, "上次启动 %s（%s前）\n",
			prev.Format("01-02 15:04"), sysinfo.HumanDuration(start.Sub(prev)))
	}

	plugins := host.Registered()
	fmt.Fprintf(&b, "启用插件 %d 个\n", len(plugins))
	for _, pl := range plugins {
		m := pl.Manifest()
		fmt.Fprintf(&b, "· %s v%s", m.Name, m.Version)
		if cmds := commandHints(m.Commands); cmds != "" {
			fmt.Fprintf(&b, "（%s）", cmds)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func commandHints(cmds []host.Command) string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		switch {
		case c.Prefix != "":
			out = append(out, c.Prefix)
		case c.Pattern != "":
			out = append(out, "/"+c.Pattern+"/")
		}
	}
	return strings.Join(out, "、")
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

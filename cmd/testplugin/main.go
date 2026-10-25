// Command testplugin 是线协议的测试夹具，**不随发布走**。
//
// 它不是一个有意义的插件：每条命令都只为验证一件事——某个 host.* 方法
// 能不能跨进程往返、错误码能不能还原成本地哨兵、1–3 MB 的图片能不能单帧
// 过去、1000 个并发请求的 id 会不会串。所以它的「业务」就是断言。
//
// 判定结果一律塞进 Reply.Content（形如 `k=v k2=v2`），由测试逐字比对：
// 只靠「有没有报错」判断代码路径，等于给「悄悄返回了零值」留后门。
//
// 用 `go build -o ... ./cmd/testplugin` 造出可执行文件，由 pkg/rpcsdk 的
// 端到端用例 spawn（ADR-0005：插件就是独立进程，测试也不该假装它是个库）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpcsdk"
)

func main() {
	if err := rpcsdk.Serve(&fixture{}); err != nil {
		fmt.Fprintln(os.Stderr, "testplugin:", err)
		os.Exit(1)
	}
}

type fixture struct {
	mu      sync.Mutex
	h       host.Host
	lastJob host.JobSpec
	started bool
}

// Start / Close / JobRunner 都实现，为的是让 plugin.manifest 的 Optional
// 报出三个方法名——那几个方法在线上是否存在，只有这条路能验证。
func (f *fixture) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}

func (f *fixture) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = false
	return nil
}

func (f *fixture) RunJob(_ context.Context, spec host.JobSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastJob = spec
	return nil
}

func (f *fixture) Manifest() host.Manifest {
	return host.Manifest{
		Name:       "testplugin",
		Version:    "0.0.0",
		APIVersion: host.APIVersion,
		// 使用面由 tools/apisurface 生成（#56），这里是手写的等价值：产物要
		// 证明它确实穿过 `--manifest` 的线协议（见 pkg/rpcsdk 的探测用例）。
		// 只列真实引用到的成员——/echo 读 Event.Args。
		Uses:    []string{"Event.Args"},
		Intents: []string{"message", "notice"},
		Commands: []host.Command{
			{Prefix: "/echo", Description: "回显参数"},
			{Prefix: "/nil"},
			{Prefix: "/fail"},
			{Prefix: "/panic"},
			{Prefix: "/render"},
			{Prefix: "/quota"},
			{Prefix: "/session"},
			{Prefix: "/rate"},
			{Prefix: "/http429"},
			{Prefix: "/kv"},
			{Prefix: "/query"},
			{Prefix: "/admins"},
			{Prefix: "/isadmin"},
			{Prefix: "/config"},
			{Prefix: "/schedule"},
			{Prefix: "/job"},
			{Prefix: "/name"},
		},
		// 声明即必填（校验在核心侧）：用例给的是 greeting="hi"、n=7。
		ConfigSchema: map[string]string{"greeting": "string", "n": "int"},
		Permission:   host.PermGroupMember,
		Yield:        true,
	}
}

func (f *fixture) Init(_ context.Context, h host.Host) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.h = h
	// 日志通道的验收点（#48）：rpcsdk 应把默认 slog 换成 stderr 上的
	// JSONHandler，核心才能按真实等级重放。等级与一个 attr 各留一处，
	// 形状对不对由 pkg/rpcsdk 的用例逐字判定。
	slog.Warn("testplugin 已初始化", "n", h.Config().Int("n", 0))
	return nil
}

func (f *fixture) host() host.Host {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.h
}

func (f *fixture) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	h := f.host()
	switch ev.Command {
	case "/echo":
		// 并发用例靠它：回显必须与请求一一对应，串了就是 id 复用。
		return &host.Reply{Content: ev.Args}, nil

	case "/nil":
		// 进程内 nil 表示「这条不回复」，与空 Reply 是两件事。
		return nil, nil

	case "/fail":
		return nil, errors.New("fixture 故意失败")

	case "/panic":
		panic("fixture 故意 panic")

	case "/render":
		img, err := h.Renderer().Markdown(ctx, "big")
		if err != nil {
			return &host.Reply{Content: "err=" + err.Error()}, nil
		}
		sum := sha256.Sum256(img.Data())
		// 长度与哈希一起回传：核心侧拿到 Reply.Image 后自己再算一遍，
		// 两个方向（host→插件、插件→核心）的正文槽都被验证到。
		return &host.Reply{
			Content: fmt.Sprintf("render=%d sha=%s", len(img.Data()), hex.EncodeToString(sum[:])),
			Image:   img,
		}, nil

	case "/quota":
		// 把错误原样抛回核心：这条路径验证「实现自定义码」的往返——
		// 插件不认识这个码（它是 core.ErrPassiveExhausted），
		// 但必须原码转发，不能塌成 CodeUnknown。
		return nil, h.Sender().Reply(ctx, ev, host.Reply{Content: "x"})

	case "/session":
		err := h.Session().Await(ctx, ev, []byte(`{"step":1}`), 0)
		return &host.Reply{Content: fmt.Sprintf("sessionBusy=%t", errors.Is(err, host.ErrSessionBusy))}, nil

	case "/rate":
		err := h.Sender().SendActive(ctx, ev.Scene, ev.PeerID, host.Reply{Content: "x"})
		return &host.Reply{Content: fmt.Sprintf("rateLimited=%t activeDisabled=%t",
			errors.Is(err, host.ErrRateLimited), errors.Is(err, host.ErrActiveDisabled))}, nil

	case "/http429":
		var out map[string]any
		err := h.HTTP().GetJSON(ctx, "https://example.invalid/x", &out)
		var status *host.HTTPStatusError
		ok := errors.As(err, &status) && status.Status == 429
		return &host.Reply{Content: fmt.Sprintf("http429=%t body=%s", ok, bodyOf(err))}, nil

	case "/kv":
		kv := h.KV()
		if err := kv.Set(ctx, "k", []byte("v1"), 0); err != nil {
			return &host.Reply{Content: "set=" + err.Error()}, nil
		}
		val, found, err := kv.Get(ctx, "k")
		if err != nil {
			return &host.Reply{Content: "get=" + err.Error()}, nil
		}
		// PeerKV 与 KV 必须是两套命名空间：peer 维度的键不能落进全局键空间。
		pkv := h.PeerKV(ev.PeerID)
		_, peerFound, err := pkv.Get(ctx, "k")
		if err != nil {
			return &host.Reply{Content: "peer=" + err.Error()}, nil
		}
		return &host.Reply{Content: fmt.Sprintf("val=%s found=%t peerFound=%t", val, found, peerFound)}, nil

	case "/query":
		rows, err := h.Query().Query(ctx, "select n, s from t", int64(7))
		if err != nil {
			return &host.Reply{Content: "err=" + err.Error()}, nil
		}
		if len(rows) == 0 {
			return &host.Reply{Content: "rows=0"}, nil
		}
		// 类型必须是 int64：JSON 默认给 float64，那会让 `row["n"].(int64)`
		// 这类断言全部失败——而且只在跨进程时失败。
		n, isInt := rows[0]["n"].(int64)
		return &host.Reply{Content: fmt.Sprintf("rows=%d int64=%t n=%d", len(rows), isInt, n)}, nil

	case "/admins":
		targets, err := h.Admins().Targets(ctx)
		if err != nil {
			return &host.Reply{Content: "err=" + err.Error()}, nil
		}
		return &host.Reply{Content: "admins=" + strconv.Itoa(len(targets))}, nil

	case "/isadmin":
		ok, err := h.IsAdmin(ctx, host.Identity{OpenID: "u1", Scene: host.SceneGroup, PeerID: ev.PeerID})
		if err != nil {
			return &host.Reply{Content: "err=" + err.Error()}, nil
		}
		return &host.Reply{Content: fmt.Sprintf("admin=%t", ok)}, nil

	case "/config":
		cfg := h.Config()
		return &host.Reply{Content: fmt.Sprintf("greeting=%s n=%d name=%s",
			cfg.Str("greeting", "<none>"), cfg.Int("n", -1), h.Plugin())}, nil

	case "/schedule":
		err := h.Scheduler().Register(host.JobSpec{Name: "tick", Cron: "@every 1m", Timeout: 60 * time.Second})
		return &host.Reply{Content: fmt.Sprintf("scheduled=%t", err == nil)}, nil

	case "/name":
		return &host.Reply{Content: "name=" + h.Plugin()}, nil

	case "/job":
		// RunJob 收到的 JobSpec 要能被回读：Peer / PerPeer / Timeout 三个字段
		// 走的是 JSON，而 Duration 的单位最容易在编码时丢掉。
		f.mu.Lock()
		spec, started := f.lastJob, f.started
		f.mu.Unlock()
		return &host.Reply{Content: fmt.Sprintf("job=%s peer=%s perPeer=%t timeout=%d started=%t",
			spec.Name, spec.Peer, spec.PerPeer, int64(spec.Timeout), started)}, nil
	}
	return nil, fmt.Errorf("testplugin: 未知命令 %q", ev.Command)
}

// bodyOf 取 HTTP 错误里的 body：契约明写它只供排障，但 Torn 的错误码在
// body 里，所以这条通道必须能过（含 body 的截断与不转义）。
func bodyOf(err error) string {
	var status *host.HTTPStatusError
	if errors.As(err, &status) {
		return status.Body
	}
	return "<none>"
}

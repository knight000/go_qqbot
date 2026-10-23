package pluginproc_test

// 本套用例的「插件」是**测试二进制自己**：TestMain 先看 os.Args[0] 的
// 文件名，命中某个角色就当插件跑 rpcsdk.Serve，否则正常跑测试。
//
// 为什么不用 go build 造夹具（pkg/rpcsdk 那套的做法）：Windows 上每次
// go build 要几秒，而这里要验的是进程管理——启动即崩、卡死、超时、被杀，
// 这些形态都必须是真的子进程（接口替身会把要验的东西全绕过去）。re-exec
// 既是真的 exec，又是零编译成本：install 只是把测试二进制复制成
// bin/<角色>，角色由文件名决定（文件名对插件来说是纯标签，正好用上）。
//
// 判定结果一律落在文件里（HELPER_MARKER / HELPER_PID），由测试读文件比对：
// 只看「没报错」等于给「悄悄什么都没做」留后门。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpcsdk"
)

const (
	markerEnv = "HELPER_MARKER"
	pidEnv    = "HELPER_PID"
)

// probeForTest 是不验超时的用例统一给的探测期限。
//
// 2 秒是发布口径（ADR-0005 决策 5），不是本机的测试口径：`go test ./...`
// 会并行跑几个包，这时冷启一个 5 MB 的测试二进制（杀软扫描 + CPU 被占满）
// 超过 2 秒是常事，于是"探测失败"变成随机红。探测超时本身由
// TestLoadKillsProbeThatHangsPastTimeout 用 1 秒专门盯着。
const probeForTest = 10 * time.Second

// 角色名。文件名（去掉 .exe）就是角色，见 helperMode。
const (
	modeEcho         = "echo"         // 正常插件：回显、记账、写 stderr
	modeSlow         = "slow"         // Handle 会卡死的插件
	modeCrashy       = "crashy"       // 启动即崩（退出码 3）
	modeBadAPI       = "badapi"       // api_version 不匹配
	modeReserved     = "reserved"     // 声明核心保留前缀 /reset
	modeHangManifest = "hangmanifest" // --manifest 卡死
	modeGarbage      = "garbage"      // --manifest 输出非 JSON
	// modeAlias 自称 "echo"：验「两个文件抢同一个插件名」。文件名只是标签，
	// 名字由 manifest 说了算，所以这种撞车是可能的（升级残留一个旧文件）。
	modeAlias = "alias"
	// modeBadInit 的 Init 返回错误：这是"配置写错"，与"产物崩溃"的处置不同。
	modeBadInit = "badinit"
	// modeStubborn 无视 plugin.close，并且在 Serve 返回后装死：三级关闭的
	// 第二、三级（EOF / 信号）必须还能把它收掉。
	modeStubborn = "stubborn"
	// modePlain 只实现 host.Plugin 的三个必选接口，不实现任何可选接口。
	modePlain = "plain"
)

func TestMain(m *testing.M) {
	if mode := helperMode(); mode != "" {
		runAsPlugin(mode)
		return
	}
	os.Exit(m.Run())
}

// helperMode 用可执行文件名判定角色。测试二进制本身叫 pluginproc.test，
// 落到 default 分支——所以正常跑测试时不会被截胡。
func helperMode() string {
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	switch base {
	case modeEcho, modeSlow, modeCrashy, modeBadAPI, modeReserved, modeHangManifest, modeGarbage,
		modeAlias, modeBadInit, modeStubborn, modePlain:
		return base
	}
	return ""
}

func hasManifestFlag() bool {
	for _, a := range os.Args[1:] {
		if a == rpcsdk.ManifestFlag {
			return true
		}
	}
	return false
}

// runAsPlugin 以插件身份跑到底，不返回。
func runAsPlugin(mode string) {
	// 每个角色写自己的 pid 文件：同一轮里可能同时起好几个探测进程，
	// 共用一个文件会互相覆盖，孤儿检查就失去意义。
	if dir := os.Getenv(pidEnv); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, mode+".pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	}

	// 探测阶段的行为：这些角色要证明"探测失败也只是跳过"，所以必须在
	// rpcsdk 之前拦下来，让 --manifest 这条路真的超时/真的吐垃圾。
	if hasManifestFlag() {
		appendMarker("probe=" + mode)
		switch mode {
		case modeHangManifest:
			time.Sleep(30 * time.Second)
			return
		case modeGarbage:
			// 最常见的现场：先打一行版本号，JSON 压根没写。
			fmt.Println(mode + " v9.9.9")
			return
		}
	}

	// 启动即崩：manifest 能应答（否则它连探测都过不了，测的就不是崩溃重启），
	// 正常服务时立刻非零退出。
	if mode == modeCrashy && !hasManifestFlag() {
		fmt.Fprintln(os.Stderr, "crashy: 启动即崩")
		os.Exit(3)
	}

	var p host.Plugin = newHelperPlugin(mode)
	if mode == modePlain {
		p = plainPlugin{h: p.(*helperPlugin)}
	}
	if err := rpcsdk.Serve(p); err != nil {
		fmt.Fprintln(os.Stderr, mode+":", err)
		os.Exit(1)
	}
	if mode == modeStubborn && !hasManifestFlag() {
		// 装死：连 EOF 都不理会（用 Sleep 而不是 select{}——后者会被 Go 的
		// 死锁检测判成 "all goroutines are asleep" 直接 panic 退出，
		// 那就不是"杀不掉"而是"自己死了"，验不到第二三级）。
		time.Sleep(30 * time.Second)
	}
}

// appendMarker 往 HELPER_MARKER 追加一行。没设置就什么也不做（有些用例不关心）。
func appendMarker(line string) {
	path := os.Getenv(markerEnv)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

// helperPlugin 是一个真正实现了 host.Plugin 三个必选接口 + 三个可选接口的
// 插件；每个角色的差别只在 manifest 与几处行为开关。
type helperPlugin struct {
	mode string

	mu  sync.Mutex
	h   host.Host
	cfg string
}

func newHelperPlugin(mode string) *helperPlugin { return &helperPlugin{mode: mode} }

func (p *helperPlugin) Manifest() host.Manifest {
	switch p.mode {
	case modeBadAPI:
		return host.Manifest{
			Name:       modeBadAPI,
			APIVersion: "999",
			Intents:    []string{"message"},
			Commands:   []host.Command{{Prefix: "/x"}},
		}
	case modeReserved:
		return host.Manifest{
			Name:       modeReserved,
			APIVersion: host.APIVersion,
			Intents:    []string{"message"},
			Commands:   []host.Command{{Prefix: "/reset"}},
		}
	}
	name := p.mode
	if p.mode == modeAlias {
		name = modeEcho
	}
	return host.Manifest{
		Name:       name,
		Version:    "0.0.0",
		APIVersion: host.APIVersion,
		Intents:    []string{"message"},
		Commands: []host.Command{
			{Prefix: "/echo", Description: "回显参数"},
			{Prefix: "/cfg", Description: "回显配置快照"},
			{Prefix: "/slow", Description: "按参数决定卡死"},
		},
	}
}

func (p *helperPlugin) Init(_ context.Context, h host.Host) error {
	if p.mode == modeBadInit {
		return errors.New("配置里的 token 为空")
	}
	p.mu.Lock()
	p.h, p.cfg = h, h.Config().Str("greeting", "<none>")
	p.mu.Unlock()
	// 逐行转发要验的是"每行都在、都带 plugin 前缀"，所以这里刻意打两行。
	fmt.Fprintln(os.Stderr, "init: 第一行")
	fmt.Fprintln(os.Stderr, "init: 第二行")
	appendMarker("init=" + p.mode + " greeting=" + p.cfg + " name=" + h.Plugin())
	return nil
}

func (p *helperPlugin) Start(context.Context) error {
	appendMarker("start=" + p.mode)
	return nil
}

// Close 是三级关闭的第一级（RPC close）：它写了 marker，才证明核心是先
// 请插件自己收摊、再动信号。
func (p *helperPlugin) Close(context.Context) error {
	if p.mode == modeStubborn {
		// 不理会 close：让核心的 1s 期限到期，走到第二、三级。
		time.Sleep(30 * time.Second)
	}
	appendMarker("close=" + p.mode)
	return nil
}

func (p *helperPlugin) RunJob(_ context.Context, spec host.JobSpec) error {
	appendMarker("job=" + spec.Name + " peer=" + spec.Peer)
	if spec.Payload == "hang" {
		time.Sleep(30 * time.Second)
	}
	return nil
}

func (p *helperPlugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	switch ev.Command {
	case "/cfg":
		p.mu.Lock()
		cfg := p.cfg
		p.mu.Unlock()
		return &host.Reply{Content: cfg}, nil
	case "/slow":
		if ev.Args == "hang" {
			// 真卡死：不理会 ctx，直到进程被杀。这才是"插件卡住"的现场。
			time.Sleep(30 * time.Second)
			return nil, ctx.Err()
		}
		return &host.Reply{Content: "slow-ok"}, nil
	default:
		return &host.Reply{Content: ev.Args}, nil
	}
}

// plainPlugin 只暴露 host.Plugin 的三个必选方法：Start / Close / RunJob
// 都不实现，手工转发三条，避免嵌入把可选接口一起"继承"过来。
type plainPlugin struct{ h *helperPlugin }

func (p plainPlugin) Manifest() host.Manifest { return p.h.Manifest() }

func (p plainPlugin) Init(ctx context.Context, h host.Host) error { return p.h.Init(ctx, h) }

func (p plainPlugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	return p.h.Handle(ctx, ev)
}

// install 把测试二进制复制成 bin/<mode>，返回落地的路径。
//
// 复制而不是 go build：角色由文件名决定，所以「造一个插件」= 一次几百毫秒
// 的文件拷贝，而不是一次编译。
func install(t *testing.T, dir, mode string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建插件目录: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制: %v", err)
	}
	blob, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("读取测试二进制: %v", err)
	}
	name := mode
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, blob, 0o755); err != nil {
		t.Fatalf("落地插件 %s: %v", mode, err)
	}
	return dst
}

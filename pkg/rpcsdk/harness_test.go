package rpcsdk_test

// 这套用例**spawn** cmd/testplugin，而不是 import 它——这不是偷懒，
// 是 ADR-0005 的全部要义：插件是一个独立可执行文件，它与核心之间只有
// 一条 stdio 管道。用库的方式测会绕过进程边界（也就绕过了本票要验证的
// 一切：握手、半包、1 MB 级的单帧、并发 id）。
//
// 夹具的判定结果都写在 Reply.Content 里（形如 `k=v`），测试逐字比对：
// 只看「有没有报错」等于给「悄悄返回零值」留后门。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
)

// errPassiveExhausted 站位于 internal/core 的 ErrPassiveExhausted。
//
// 那个哨兵**故意不进契约**（#36），所以 pkg/rpc 不能认识它；能验证的是机制：
// 核心在实现自定义段注册自己的码，插件不认识它也必须原码转发回来。
// #40 把 core 接上来时登记的正是这个号码（rpc.CodeImplBase）。
var errPassiveExhausted = errors.New("被动回复窗口已用尽（测试哨兵）")

// harness 是一台「最小可用的核心」：真实的 host.Service + 全部替身后端。
// 用 host.NewService 而不是手写 host.Host，是为了让线协议跑在**生产那条
// 装配路径**上——手写替身会漏掉「typed-nil 让 x == nil 永不成立」这类陷阱。
type harness struct {
	reg  *rpc.Registry
	host *host.Service

	mu    sync.Mutex
	kv    map[string][]byte
	specs []host.JobSpec
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	reg := rpc.NewRegistry()
	if err := reg.Register(rpc.CodeImplBase, errPassiveExhausted); err != nil {
		t.Fatalf("注册实现自定义码: %v", err)
	}
	h := &harness{reg: reg, kv: make(map[string][]byte)}
	h.host = host.NewService(host.Options{
		Name: "testplugin",
		// 与夹具的 ConfigSchema 对应：声明即必填，值必须都在。
		Config:   host.NewParams("testplugin", map[string]any{"greeting": "hi", "n": int64(7)}),
		Sender:   fakeSender{},
		KV:       (*fakeKV)(h),
		DB:       fakeDB{},
		Admin:    fakeAdmin{},
		HTTP:     fakeHTTP{},
		Sched:    (*fakeSched)(h),
		Renderer: fakeRenderer{},
		Session:  fakeSession{},
	})
	return h
}

func (h *harness) scheduled() []host.JobSpec {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]host.JobSpec(nil), h.specs...)
}

// —— 后端替身 ——

type fakeSender struct{}

func (fakeSender) Reply(context.Context, *host.Event, host.Reply) error { return errPassiveExhausted }
func (fakeSender) SendActive(context.Context, host.Scene, string, host.Reply) error {
	return host.ErrRateLimited
}

type fakeKV harness

func (k *fakeKV) KVGet(_ context.Context, plugin, peer, key string) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.kv[plugin+"|"+peer+"|"+key]
	return v, ok, nil
}

func (k *fakeKV) KVSet(_ context.Context, plugin, peer, key string, val []byte, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.kv[plugin+"|"+peer+"|"+key] = val
	return nil
}

func (k *fakeKV) KVDelete(_ context.Context, plugin, peer, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.kv, plugin+"|"+peer+"|"+key)
	return nil
}

type fakeDB struct{}

// QueryRows 顺手断言 SQL 参数保真：int64 过一趟 JSON 若变成 float64，
// `WHERE id = ?` 这类比较就会在 SQLite 里走 REAL 路径。
func (fakeDB) QueryRows(_ context.Context, _ string, args ...any) ([]map[string]any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("参数个数 %d，期望 1", len(args))
	}
	n, ok := args[0].(int64)
	if !ok || n != 7 {
		return nil, fmt.Errorf("SQL 参数 = %#v（%T），期望 int64(7)", args[0], args[0])
	}
	return []map[string]any{{"n": int64(42), "s": "x"}}, nil
}

type fakeAdmin struct{}

func (fakeAdmin) AdminRole(context.Context, string, string) (string, bool, error) {
	return "admin", true, nil
}
func (fakeAdmin) AdminTargets(context.Context) ([]host.AdminTarget, error) {
	return []host.AdminTarget{
		{Role: "owner", C2COpenID: "o1"},
		{Role: "admin", C2COpenID: "a1"},
	}, nil
}

type fakeHTTP struct{}

func (fakeHTTP) GetJSON(context.Context, string, any) error {
	return &host.HTTPStatusError{Status: 429, Body: `{"error":{"code":16}}`}
}
func (fakeHTTP) PostJSON(context.Context, string, any, any) error { return nil }
func (fakeHTTP) GetJSONWithHeader(context.Context, string, map[string]string, any) error {
	return nil
}
func (fakeHTTP) GetJSONOnce(context.Context, string, map[string]string, any) error { return nil }
func (f fakeHTTP) WithTimeout(time.Duration) host.HTTPBackend                      { return f }

type fakeSched harness

func (s *fakeSched) Register(spec host.JobSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs = append(s.specs, spec)
	return nil
}

// bigPayload 是 3 MB 假图片的确定性字节模式：两端各算一次哈希，
// 长度对得上而内容被截断的情形只有比哈希才抓得到。
func bigPayload() []byte {
	const size = 3 << 20
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

type fakeRenderer struct{}

func (fakeRenderer) Markdown(context.Context, string) (host.Image, error) {
	return host.ImageFromBytes(bigPayload()), nil
}

type fakeSession struct{}

func (fakeSession) AwaitSession(context.Context, string, *host.Event, []byte, time.Duration) error {
	return host.ErrSessionBusy
}

// —— spawn ——

const fixturePkg = "./cmd/testplugin"

var (
	buildOnce sync.Once
	buildBin  string
	buildErr  error
)

// fixtureBinary 只在第一次调用时编一次。产物放仓库内的 .temp/（gitignore），
// 与仓库「临时文件不进系统临时目录」的约定一致。
func fixtureBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			buildErr = err
			return
		}
		dir := filepath.Join(root, ".temp")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			buildErr = err
			return
		}
		name := "testplugin"
		if os.PathSeparator == '\\' {
			name += ".exe"
		}
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", out, fixturePkg)
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			buildErr = fmt.Errorf("go build %s: %w\n%s", fixturePkg, err, stderr.String())
			return
		}
		buildBin = out
	})
	if buildErr != nil {
		t.Fatalf("编译夹具: %v", buildErr)
	}
	return buildBin
}

// plugin 是一次已经握手完成的子进程。
type plugin struct {
	t        *testing.T
	conn     *rpc.Conn
	manifest host.Manifest
	optional []string
	cmd      *exec.Cmd
	// stderr 收着插件写在标准错误上的全部输出。只能在进程退出之后读：
	// exec 用内部协程往里抄，提前读就是一场数据竞争。
	stderr *bytes.Buffer
}

// stderrAfterStop 收掉子进程并返回它的 stderr 全文。
//
// 判定插件日志通道（#48）必须看真实的管道内容——handler 设没设、设成什么，
// 在核心眼里只有"stderr 上出现了什么"这一个事实。
func (p *plugin) stderrAfterStop() string {
	p.t.Helper()
	_ = p.conn.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	// Wait 会等 stderr 的抄写协程结束，之后读缓冲区才安全。
	// 重复 Wait 只返回错误，用例结尾的清理照样能收干净。
	_ = p.cmd.Wait()
	return p.stderr.String()
}

// startPlugin 起子进程、接好两条管道、做完 init。任何一步失败都是致命错误。
func startPlugin(t *testing.T, h *harness) *plugin {
	t.Helper()
	cmd := exec.Command(fixtureBinary(t))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动夹具: %v", err)
	}

	conn := rpc.NewConn(stdout, stdin, rpc.Options{Registry: h.reg})
	if err := rpc.ServeHost(conn, h.host); err != nil {
		t.Fatalf("挂载 host.*: %v", err)
	}
	go func() { _ = conn.Run() }()

	t.Cleanup(func() {
		_ = conn.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		// stderr 要等 Wait 之后才能读：exec 用一个协程往里拷，
		// 提前读就是一场数据竞争。
		if t.Failed() {
			t.Logf("插件 stderr:\n%s", stderr.String())
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m, optional, err := conn.CallManifest(ctx)
	if err != nil {
		t.Fatalf("握手: %v", err)
	}
	if err := conn.CallInit(ctx, "testplugin", map[string]any{"greeting": "hi", "n": int64(7)}); err != nil {
		t.Fatalf("init: %v", err)
	}
	return &plugin{t: t, conn: conn, manifest: m, optional: optional, cmd: cmd, stderr: &stderr}
}

// event 造一条进站消息。Command / Args 由核心在路由阶段填好（契约 8），
// 夹具不再自己 CutPrefix——所以这里直接给最终值。
func (p *plugin) event(command, args string) *host.Event {
	return &host.Event{
		ID:        "m-" + command + "-" + args,
		Kind:      host.KindMessage,
		Scene:     host.SceneGroup,
		PeerID:    "g1",
		SenderID:  "u1",
		Content:   command + " " + args,
		Command:   command,
		Args:      args,
		MsgID:     "m1",
		Timestamp: time.Now(),
		Actor: host.Principal{
			OpenID: "u1", Scene: host.SceneGroup, PeerID: "g1",
			Role: host.RoleUser, GroupRole: host.GroupRoleMember,
		},
	}
}

// send 投递一条命令并返回插件的回复。reply 为 nil 表示插件选择不回复。
func (p *plugin) send(command, args string) *host.Reply {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	reply, err := p.conn.CallHandle(ctx, p.event(command, args))
	if err != nil {
		p.t.Fatalf("%s: %v", command, err)
	}
	return reply
}

// sendErr 是「预期会失败」的投递：错误是断言对象，不能让用例提前挂掉。
func (p *plugin) sendErr(command string) error {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := p.conn.CallHandle(ctx, p.event(command, ""))
	return err
}

// verdict 要求插件回了一句话，并返回它的正文。
func (p *plugin) verdict(command, args string) string {
	p.t.Helper()
	reply := p.send(command, args)
	if reply == nil {
		p.t.Fatalf("%s: 插件没有回复", command)
	}
	return reply.Content
}

// expect 是一条用例的常用形态：跑一条命令，逐字比对正文。
func (p *plugin) expect(command, args, want string) {
	p.t.Helper()
	if got := p.verdict(command, args); got != want {
		p.t.Fatalf("%s 的结果 = %q，期望 %q", command, got, want)
	}
}

// ensure 让 reflect.DeepEqual 的失败信息带上「期望/实际」。
func ensure[T any](t *testing.T, got, want T, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %#v，期望 %#v", what, got, want)
	}
}

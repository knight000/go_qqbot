package rpcsdk_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
)

// TestHandshake 覆盖「装上一个插件」这一步拿到的全部事实。
//
// 清单是核心唯一能静态校验的东西（§3.2）：字段丢一个，表现就是某条规则
// 不生效而没有报错。ConfigSchema 尤其容易被漏——它一旦在线上丢了，
// 装配期校验就变成空转。
func TestHandshake(t *testing.T) {
	h := newHarness(t)
	p := startPlugin(t, h)

	ensure(t, p.manifest.Name, "testplugin", "Manifest.Name")
	ensure(t, p.manifest.APIVersion, host.APIVersion, "Manifest.APIVersion")
	ensure(t, p.manifest.ConfigSchema, map[string]string{"greeting": "string", "n": "int"}, "Manifest.ConfigSchema")
	ensure(t, p.manifest.Permission, host.PermGroupMember, "Manifest.Permission")
	ensure(t, p.manifest.Yield, true, "Manifest.Yield")
	ensure(t, p.manifest.Intents, []string{"message", "notice"}, "Manifest.Intents")
	if len(p.manifest.Commands) != 17 {
		t.Fatalf("命令数 = %d，期望 17", len(p.manifest.Commands))
	}
	ensure(t, p.manifest.Commands[0].Prefix, "/echo", "Commands[0].Prefix")

	// 可选接口是断言出来的，跨进程只能靠清单告诉核心。
	ensure(t, p.optional, []string{rpc.OptionalStart, rpc.OptionalClose, rpc.OptionalRunJob}, "Optional")

	// init 里的配置是快照：整数必须还是整数（Transient 3.0 事故的现场）。
	p.expect("/config", "", "greeting=hi n=7 name=testplugin")
}

// TestHandleEcho 是最短的一条链路：事件进去、Reply 出来。
func TestHandleEcho(t *testing.T) {
	h := newHarness(t)
	p := startPlugin(t, h)
	p.expect("/echo", "你好", "你好")
	// 同一连接上多来几次，确认没有「只第一帧对」这种错位。
	for i := 0; i < 5; i++ {
		p.expect("/echo", fmt.Sprint(i), fmt.Sprint(i))
	}
}

// TestNilReply：nil 与空 Reply 必须能区分。进程内 nil 表示「这条不回复」，
// 空 Reply 会让核心去发一条空消息——两者混同就是「机器人偶尔发空消息」。
func TestNilReply(t *testing.T) {
	h := newHarness(t)
	p := startPlugin(t, h)
	if reply := p.send("/nil", ""); reply != nil {
		t.Fatalf("nil 回复变成了 %+v", reply)
	}
}

// TestErrorCodes 是验收里的错误码往返。
//
// 每条都在**插件侧**做判定（errors.Is / errors.As），然后把结论带回来：
// 只有这样才证明「还原成本地哨兵」这件事在对面真的发生了。
func TestErrorCodes(t *testing.T) {
	h := newHarness(t)
	p := startPlugin(t, h)

	t.Run("实现自定义码原码转发", func(t *testing.T) {
		// 本票验收点名的 ErrPassiveExhausted：它留在 core（#36），
		// 插件不认识 1000 号，但必须原码转发回来，核心才认得出自己。
		err := p.sendErr("/quota")
		if !errors.Is(err, errPassiveExhausted) {
			t.Fatalf("err = %v，期望命中 errPassiveExhausted", err)
		}
	})

	t.Run("会话被占用", func(t *testing.T) {
		p.expect("/session", "", "sessionBusy=true")
	})

	t.Run("平台限流", func(t *testing.T) {
		p.expect("/rate", "", "rateLimited=true activeDisabled=false")
	})

	t.Run("HTTP 429 带 body", func(t *testing.T) {
		p.expect("/http429", "", `http429=true body={"error":{"code":16}}`)
	})

	t.Run("未分类错误", func(t *testing.T) {
		err := p.sendErr("/fail")
		if err == nil || !strings.Contains(err.Error(), "fixture 故意失败") {
			t.Fatalf("err = %v，期望带上原文", err)
		}
		if errors.Is(err, errPassiveExhausted) || errors.Is(err, host.ErrRateLimited) {
			t.Fatalf("未分类错误被误判成哨兵: %v", err)
		}
	})

	t.Run("handler panic 不打断连接", func(t *testing.T) {
		err := p.sendErr("/panic")
		if !errors.Is(err, rpc.ErrHandlerPanic) {
			t.Fatalf("err = %v，期望 ErrHandlerPanic", err)
		}
		// panic 之后连接必须还能用。
		p.expect("/echo", "alive", "alive")
	})
}

// TestLargePayload 是「3 MB 单帧往返」的验收项。
//
// 两个方向都走一遍：host.render 的应答带 3 MB 正文（核心 → 插件），
// 紧接着 Reply.Image 又把同一份字节带回来（插件 → 核心）。夹具把收到的
// 长度与哈希写在 Content 里，核心侧再算一次自己的。
func TestLargePayload(t *testing.T) {
	h := newHarness(t)
	p := startPlugin(t, h)

	reply := p.send("/render", "")
	if reply == nil {
		t.Fatal("没有回复")
	}
	payload := bigPayload()
	sum := sha256.Sum256(payload)
	want := fmt.Sprintf("render=%d sha=%x", len(payload), sum)
	if reply.Content != want {
		t.Fatalf("插件看到的是 %q，期望 %q", reply.Content, want)
	}
	got := reply.Image.Data()
	if len(got) != len(payload) {
		t.Fatalf("回来的图片 %d 字节，期望 %d", len(got), len(payload))
	}
	if gotSum := sha256.Sum256(got); gotSum != sum {
		t.Fatal("回来的图片内容被改过")
	}
	if reply.Image.Source() != host.ImageSourceBytes {
		t.Fatalf("图片来源 = %q", reply.Image.Source())
	}
}

// TestConcurrentHandles 是验收里的「1000 个并发小请求 id 不串」。
//
// 串了的表现极隐蔽：回应对象永远不报错，只是偶发地把别人的结果交给
// 这个调用方。所以每个请求带一个不同的回显值，逐条比对。
func TestConcurrentHandles(t *testing.T) {
	if testing.Short() {
		t.Skip("并发用例在 -short 下跳过")
	}
	h := newHarness(t)
	p := startPlugin(t, h)

	const n = 1000
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprint(i)
			reply, err := p.conn.CallHandle(ctx, p.event("/echo", want))
			if err != nil {
				errs <- fmt.Errorf("第 %d 个: %w", i, err)
				return
			}
			if reply == nil || reply.Content != want {
				errs <- fmt.Errorf("第 %d 个拿到 %v", i, reply)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		total := len(failures)
		if len(failures) > 3 {
			failures = failures[:3]
		}
		t.Fatalf("%d 个请求出错，前几个: %v", total, failures)
	}
}

// TestHostCapabilities 把剩下几个 host.* 方法一次过一遍：
// KV / PeerKV / Query / Admins / IsAdmin / Scheduler。
func TestHostCapabilities(t *testing.T) {
	h := newHarness(t)
	p := startPlugin(t, h)

	t.Run("KV 与 PeerKV 是两套命名空间", func(t *testing.T) {
		p.expect("/kv", "", "val=v1 found=true peerFound=false")
	})
	t.Run("查询结果保持 int64", func(t *testing.T) {
		p.expect("/query", "", "rows=1 int64=true n=42")
	})
	t.Run("管理员目录", func(t *testing.T) {
		p.expect("/admins", "", "admins=2")
	})
	t.Run("IsAdmin", func(t *testing.T) {
		p.expect("/isadmin", "", "admin=true")
	})
	t.Run("注册任务时核心补 Plugin 名", func(t *testing.T) {
		p.expect("/schedule", "", "scheduled=true")
		specs := h.scheduled()
		if len(specs) != 1 {
			t.Fatalf("注册了 %d 个任务", len(specs))
		}
		ensure(t, specs[0].Plugin, "testplugin", "JobSpec.Plugin（由核心填）")
		ensure(t, specs[0].Name, "tick", "JobSpec.Name")
		ensure(t, specs[0].Timeout, 60*time.Second, "JobSpec.Timeout")
	})
}

// TestLifecycle 覆盖 Start / RunJob / Close：三个可选方法在线上都存在，
// 而且 JobSpec 的 Peer / PerPeer / Timeout 三个字段没在编码时丢掉单位。
func TestLifecycle(t *testing.T) {
	h := newHarness(t)
	p := startPlugin(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := p.conn.CallStart(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	spec := host.JobSpec{
		Plugin:  "testplugin",
		Name:    "tick",
		Cron:    "@every 1m",
		Timeout: 90 * time.Second,
		PerPeer: true,
		Peer:    "g9",
	}
	if err := p.conn.CallRunJob(ctx, spec); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	p.expect("/job", "", "job=tick peer=g9 perPeer=true timeout=90000000000 started=true")

	if err := p.conn.CallClose(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p.expect("/job", "", "job=tick peer=g9 perPeer=true timeout=90000000000 started=false")
}

// TestHalfPacketOnSpawn 是验收的最后一项：对端写了半包就退出，读侧必须
// 报错而不是 panic，子进程也必须是「非零退出」而不是「卡住」。
func TestHalfPacketOnSpawn(t *testing.T) {
	cmd := exec.Command(fixtureBinary(t))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	cmd.Stdout = io.Discard
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动: %v", err)
	}
	// 只写长度前缀的一半就关掉：读侧拿到的必须是 ErrUnexpectedEOF。
	if _, err := stdin.Write([]byte{0x00, 0x00}); err != nil {
		t.Fatalf("写半包: %v", err)
	}
	_ = stdin.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("半包之后子进程竟然正常退出（应当非零退出）")
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("半包之后子进程没有退出")
	}
	out := stderr.String()
	if strings.Contains(out, "panic:") || strings.Contains(out, "goroutine ") {
		t.Fatalf("读侧 panic 了：\n%s", out)
	}
	if !strings.Contains(out, "unexpected EOF") {
		t.Fatalf("stderr 里没有 unexpected EOF：\n%s", out)
	}
}

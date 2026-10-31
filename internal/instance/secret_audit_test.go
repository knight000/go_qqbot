package instance

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/internal/store"
)

// 脱敏审计：一条运行中的进程从头到尾都不许把 [[bots]].secret 写进任何输出。
//
// 这条用例刻意跑满所有"会输出东西"的路径，而不是逐个断言某一行：
//   - 启动摘要与每个 Bot 的启动行（slog）
//   - 认领过的 Bot 收发一条消息（api 的 token 请求体里就有 secret）
//   - 出站 429 的冷却告警（错误链）
//   - 未认领 Bot 的闸门丢弃（Debug 级日志）
//   - `/claim` 的处理路径
//   - `[debug].raw` 的原始载荷与「收到」五行轨迹（写到 stdout，不是 slog）
//
// 最后一个断言是**反证**：轨迹里必须真的出现那条消息的正文。否则"日志里没有
// secret"随时可能只是因为这些路径根本没跑起来。
func TestBotSecretNeverReachesAnyOutput(t *testing.T) {
	isolateTemp(t, t.TempDir())
	stdout := captureStdout(t)

	p := newFakePlatform(t)
	cfg := testConfig(t, p, "102000001", "102000002", "102000003")
	cfg.Debug = config.Debug{Enabled: true, Level: config.DebugFull, Raw: true}
	// A 已认领（走路由与出站），B 被另一个 Core 占着（走"没起来"的错误链），
	// C 没人认领（走闸门丢弃与 /claim）。
	seedBotState(t, cfg, "102000001", core.BotStateActive)
	holdAppLock(t, "102000002")

	log, logs := verboseLogger()
	c := startContainer(t, cfg, p, func(o *Options) { o.Log = log })
	if sts := c.Statuses(); len(sts) != 3 || sts[1].Err == nil {
		t.Fatalf("预置形态不对：应有一个 Bot 启动失败，实际 %+v", sts)
	}

	p.waitReady(t, "102000001")
	const canary = "CANARY-TRACE-BODY"
	p.push(t, "102000001", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-A1", "G-A", canary))
	p.waitSends(t, 1)

	// 出站 429：冷却告警里带域名与 retry_after，不该带凭据。
	p.throttle429("102000001", 1)
	p.push(t, "102000001", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-A2", "G-A", "throttled"))
	waitLog(t, logs, "出站 429", "bot=102000001")

	// 未认领的 Bot：普通消息被闸门丢掉，/claim 放行。两者都不许打印 secret。
	p.waitReady(t, "102000003")
	p.push(t, "102000003", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-C1", "G-C", "dropped"))
	p.push(t, "102000003", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-C2", "G-C", "wrong-guess-not-the-secret"))
	waitLog(t, logs, "Bot 非 active，丢弃事件", "bot=102000003")

	// 轨迹是异步写的：等反证那条正文出现，再一次性审。
	waitStdout(t, stdout, canary)

	out, trace := logs(), stdout()

	// secret 一个字节都不许出现在两路输出里。三个 Bot 各有一份，逐个查：
	// 只查其中一个会漏掉"拿错了别人的 secret 去打日志"这种串味。
	for _, secret := range []string{"secret-102000001", "secret-102000002", "secret-102000003"} {
		if strings.Contains(out, secret) {
			t.Errorf("日志里出现了 %q：\n%s", secret, out)
		}
		if strings.Contains(trace, secret) {
			t.Errorf("调试轨迹里出现了 %q：\n%s", secret, trace)
		}
	}

	// 允许出现的：appid（定位用）与状态。
	for _, want := range []string{"bot=102000001", "bot=102000002", "bot=102000003", "共 3 个 Bot"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志里缺少 %q（定位信息不该被脱敏掉）：\n%s", want, out)
		}
	}

	// 反证：原始轨迹真的在打正文与原始载荷，否则上面那些"没有 secret"是空跑。
	if !strings.Contains(trace, "收到 群聊消息") {
		t.Errorf("调试轨迹没有打印「收到」，本用例没覆盖到轨迹路径：\n%s", trace)
	}
	if !strings.Contains(trace, "原始事件") {
		t.Errorf("调试轨迹没有打印原始载荷（[debug].raw）:\n%s", trace)
	}
}

// seedBotState 把某个 Bot 的库预置成给定状态（其余 Bot 不动）。
func seedBotState(t *testing.T, cfg *config.Config, appID string, st core.BotState) {
	t.Helper()
	for _, b := range cfg.Bots {
		if b.AppID != appID {
			continue
		}
		db, err := store.Open(cfg.BotStorePath(b))
		if err != nil {
			t.Fatalf("预置 %s 的库: %v", appID, err)
		}
		defer func() { _ = db.Close() }()
		if err := db.Migrate(context.Background()); err != nil {
			t.Fatalf("迁移 %s 的库: %v", appID, err)
		}
		if err := core.SaveBotState(context.Background(), db, st); err != nil {
			t.Fatalf("写入 %s 的状态: %v", appID, st)
		}
		return
	}
	t.Fatalf("配置里没有 %s", appID)
}

// holdAppLock 抢在容器之前占住某个 appID 的锁，模拟"另一个 Core 正在托管它"。
func holdAppLock(t *testing.T, appID string) {
	t.Helper()
	holder, err := Acquire(LockOptions{
		AppID:    appID,
		DataPath: filepath.Join(t.TempDir(), "other.db.lock"),
	})
	if err != nil {
		t.Fatalf("预占 %s 的锁: %v", appID, err)
	}
	t.Cleanup(func() { _ = holder.Release() })
}

// verboseLogger 是 Debug 级的 testLogger：闸门丢弃与 httpx 的部分告警只在
// Debug 级出现，审计要把它们一起捞进来。
func verboseLogger() (*slog.Logger, func() string) {
	var mu sync.Mutex
	var sb strings.Builder
	l := slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return sb.Write(b)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	return l, func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

// captureStdout 把进程的 stdout 换成管道并持续收集，返回读取函数。
//
// 调试轨迹（internal/core.Trace）是直接写 os.Stdout 的，不走 slog：审计
// "轨迹里有没有 secret"只能从这一层看。测试结束自动换回原 stdout。
func captureStdout(t *testing.T) func() string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道: %v", err)
	}
	os.Stdout = w

	var mu sync.Mutex
	var sb strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				mu.Lock()
				sb.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = w.Close()
		os.Stdout = old
		<-done
		_ = r.Close()
	})
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

// waitStdout 等 stdout 里出现 want 子串（最多 10s）。
func waitStdout(t *testing.T, out func() string, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := out()
		if strings.Contains(got, want) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("stdout 里等不到 %q，实际:\n%s", want, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

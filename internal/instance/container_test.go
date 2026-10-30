package instance

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/pkg/host"
)

// 两个不同 appid 的 Bot 同进程运行：各自收到消息、各自回复、各自落各自的库。
//
// 这是本票唯一真正要证明的事。断言全部落在外部可观察的地方——出站请求带的是
// 哪个 Bot 的 token、两个库里的行、各自的 bot_state——而不是容器内部字段。
func TestBotsRunIsolatedWithOwnStoreAndToken(t *testing.T) {
	p := newFakePlatform(t)
	cfg := testConfig(t, p, "102000001", "102000002")
	c := startContainer(t, cfg, p)

	p.waitReady(t, "102000001")
	p.waitReady(t, "102000002")

	p.push(t, "102000001", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-A", "G-A", "hello-A"))
	p.push(t, "102000002", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-B", "G-B", "hello-B"))

	sends := p.waitSends(t, 2)
	replies := map[string][]sendRecord{}
	for _, s := range sends {
		replies[s.appID] = append(replies[s.appID], s)
	}
	for _, tc := range []struct{ appID, wantPath, wantContent string }{
		{"102000001", "/v2/groups/G-A/messages", "echo:hello-A"},
		{"102000002", "/v2/groups/G-B/messages", "echo:hello-B"},
	} {
		got := replies[tc.appID]
		if len(got) != 1 {
			t.Fatalf("%s 应只回一条，实际 %+v（全部: %+v）", tc.appID, got, sends)
		}
		if got[0].path != tc.wantPath || got[0].content != tc.wantContent {
			t.Errorf("%s 的回复落在了错误的会话/内容: %+v", tc.appID, got[0])
		}
	}

	ctx := context.Background()
	// 交叉断言：A 的消息不出现在 B 的库里，反之亦然。
	for i, tc := range []struct {
		appID, ownEvent, otherEvent string
	}{
		{"102000001", "EV-A", "EV-B"},
		{"102000002", "EV-B", "EV-A"},
	} {
		rows, err := c.bots[i].db.QueryRows(ctx, `SELECT event_id FROM messages`)
		if err != nil {
			t.Fatalf("查 %s 的 messages: %v", tc.appID, err)
		}
		got := map[string]bool{}
		for _, row := range rows {
			got[row["event_id"].(string)] = true
		}
		if !got[tc.ownEvent] {
			t.Errorf("%s 的库里应有自己的消息 %s，实际 %v", tc.appID, tc.ownEvent, got)
		}
		if got[tc.otherEvent] {
			t.Errorf("%s 的库里出现了另一个 Bot 的消息 %s（跨租户污染）", tc.appID, tc.otherEvent)
		}
	}

	// 令牌缓存按 Bot 分区：各自的 bot_state 里只有自己的 token。
	for i, appID := range []string{"102000001", "102000002"} {
		tok, ok, err := c.bots[i].db.GetState(ctx, "access_token")
		if err != nil || !ok {
			t.Fatalf("%s 的 access_token 未落库: ok=%v err=%v", appID, ok, err)
		}
		if want := tokenFor(appID); tok != want {
			t.Errorf("%s 的 access_token = %q，期望 %q", appID, tok, want)
		}
	}

	// 进程级状态：两个都算启动成功，库路径互不相同。
	sts := c.Statuses()
	if len(sts) != 2 {
		t.Fatalf("应有 2 个 Bot 的状态，实际 %d", len(sts))
	}
	for _, st := range sts {
		if st.Err != nil {
			t.Errorf("%s 不该启动失败: %v", st.AppID, st.Err)
		}
	}
	if sts[0].DBPath == sts[1].DBPath {
		t.Fatalf("两个 Bot 的库路径不能相同: %q", sts[0].DBPath)
	}
}

// 同一 appid 被另一个 Core 占着时只拒这一个 Bot，其余照常启动，进程不退出。
func TestLockedBotDoesNotStopTheOthers(t *testing.T) {
	isolateTemp(t, t.TempDir())
	p := newFakePlatform(t)
	cfg := testConfig(t, p, "102000001", "102000002")

	// 抢在容器之前占住 B 的 appID 锁，模拟另一个 Core 正在托管它。
	holder, err := Acquire(LockOptions{
		AppID:    "102000002",
		DataPath: filepath.Join(t.TempDir(), "other.db.lock"),
	})
	if err != nil {
		t.Fatalf("预占锁: %v", err)
	}
	defer holder.Release()

	log, logs := testLogger()
	c := startContainer(t, cfg, p, func(o *Options) { o.Log = log })

	p.waitReady(t, "102000001")
	p.push(t, "102000001", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-A", "G-A", "hi"))
	if got := p.waitSends(t, 1); got[0].appID != "102000001" {
		t.Fatalf("消息应由未被占用的 Bot 回复，实际 %+v", got)
	}

	sts := c.Statuses()
	if len(sts) != 2 {
		t.Fatalf("两个 Bot 都应出现在状态里，实际 %d", len(sts))
	}
	if sts[0].Err != nil {
		t.Errorf("A 不该启动失败: %v", sts[0].Err)
	}
	if sts[1].Err == nil {
		t.Fatal("B 的 appID 被占着，应标记为未启动")
	}
	if !strings.Contains(logs(), "102000002") {
		t.Errorf("日志必须指名被拒的 Bot，实际:\n%s", logs())
	}
	// 进程没退出：另一个 Bot 还在线（它刚刚回过消息）。
	p.mu.Lock()
	_, connected := p.conns["102000001"]
	p.mu.Unlock()
	if !connected {
		t.Fatal("A 的连接不该被 B 的失败带走")
	}
}

// 单个 Bot 的连接（这里是数据目录）坏掉，不阻塞其他 Bot 启动。
func TestBrokenBotDoesNotBlockStartup(t *testing.T) {
	isolateTemp(t, t.TempDir())
	p := newFakePlatform(t)
	cfg := testConfig(t, p, "102000001", "102000002")
	// 把第一个 Bot 的 data_dir 指向"一个文件下面"：MkdirAll 必然失败。
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Bots[0].DataDir = filepath.Join(blocked, "bots", "102000001")

	c := startContainer(t, cfg, p)
	p.waitReady(t, "102000002")
	p.push(t, "102000002", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-B", "G-B", "hi"))
	if got := p.waitSends(t, 1); got[0].appID != "102000002" {
		t.Fatalf("消息应由健康的 Bot 回复，实际 %+v", got)
	}

	sts := c.Statuses()
	if sts[0].Err == nil {
		t.Error("数据目录不可用的 Bot 应标记为未启动")
	}
	if sts[1].Err != nil {
		t.Errorf("健康 Bot 不该受影响: %v", sts[1].Err)
	}
}

// 启动错峰：两个以上 Bot 时 GET /gateway 串行且存在间隔。
//
// 断言"串行 + 间隔存在"，不断言具体秒数：间隔可配，钉死 30s 只会让改默认值
// 的人去改测试。同时断言启动总时长覆盖了这段等待——错峰代价必须可见。
func TestStartupStaggersGatewayFetches(t *testing.T) {
	p := newFakePlatform(t)
	cfg := testConfig(t, p, "102000001", "102000002", "102000003")
	const interval = 40 * time.Millisecond
	cfg.Bot.GatewayStagger = &config.Duration{Duration: interval}

	startedAt := time.Now()
	startContainer(t, cfg, p)
	elapsed := time.Since(startedAt)

	calls, overlapped := p.gatewaySnapshot()
	if overlapped {
		t.Fatalf("GET /gateway 发生了重叠：串行没有生效 (%+v)", calls)
	}
	if len(calls) != 3 {
		t.Fatalf("应有 3 次取地址，实际 %+v", calls)
	}
	for i := 1; i < len(calls); i++ {
		gap := calls[i].start.Sub(calls[i-1].end)
		if gap < interval {
			t.Errorf("第 %d 次与上一次的间隔 %s 小于 %s", i+1, gap, interval)
		}
	}
	if min := interval * time.Duration(len(calls)-1); elapsed < min {
		t.Errorf("启动耗时 %s 应覆盖错峰等待（至少 %s）", elapsed, min)
	}
	for _, appID := range []string{"102000001", "102000002", "102000003"} {
		p.waitReady(t, appID)
	}
}

// 关停期间不产生新的 Bot 启动：信号落在两个 Bot 之间时，后面的 Bot
// 连库都不该开。
func TestShutdownDuringStartupLaunchesNoMoreBots(t *testing.T) {
	isolateTemp(t, t.TempDir())
	p := newFakePlatform(t)
	cfg := testConfig(t, p, "102000001", "102000002")
	cfg.Bot.GatewayStagger = &config.Duration{Duration: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 第一个 Bot 一取地址就发关停信号：此时启动循环正卡在等它的首次排队，
	// 还没轮到第二个 Bot。
	p.onGateway = func(appID string) {
		if appID == "102000001" {
			cancel()
		}
	}

	c, err := Start(ctx, Options{
		Config:    cfg,
		Log:       quietLogger(),
		Outbound:  p.httpx(),
		Handshake: p.srv.Client(),
		LoadPlugins: func(context.Context, config.Bot, *slog.Logger) ([]host.Plugin, error) {
			return []host.Plugin{&stubPlugin{name: "echo"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("启动容器: %v", err)
	}
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer scancel()
		_ = c.Shutdown(sctx)
	}()

	if sts := c.Statuses(); len(sts) > 1 {
		t.Fatalf("关停信号之后不该再装第二个 Bot，实际状态 %+v", sts)
	}
	if _, err := os.Stat(cfg.BotStorePath(cfg.Bots[1])); !os.IsNotExist(err) {
		t.Fatalf("第二个 Bot 的库不该被打开，stat: %v", err)
	}
}

// 关停顺序对 N 个 Bot 成立：网关先落盘，再关核心与插件，最后关库与放锁。
//
// 观察点：关停后每个库都能被重新打开并读到 session_id（说明"落盘早于关库"），
// 两把锁都能重新拿到（说明"关库早于放锁"之外的顺序至少没有把锁漏掉）。
func TestShutdownPersistsSessionThenReleases(t *testing.T) {
	isolateTemp(t, t.TempDir())
	p := newFakePlatform(t)
	cfg := testConfig(t, p, "102000001", "102000002")
	c := startContainer(t, cfg, p)
	p.waitReady(t, "102000001")
	p.waitReady(t, "102000002")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("关停: %v", err)
	}

	for i, appID := range []string{"102000001", "102000002"} {
		path := cfg.BotStorePath(cfg.Bots[i])
		db := openStoreForTest(t, path)
		tok, ok, err := db.GetState(context.Background(), "session_id")
		if err != nil || !ok || tok != "session-"+appID {
			t.Errorf("%s 的 session 未在关库前落盘: ok=%v v=%q err=%v", appID, ok, tok, err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("关闭校验用连接: %v", err)
		}
		lock, err := Acquire(LockOptions{AppID: appID, DataPath: path + ".lock"})
		if err != nil {
			t.Errorf("%s 的锁没有在关停后释放: %v", appID, err)
			continue
		}
		_ = lock.Release()
	}
}

// 全部 Bot 的网关都永久失败时进程退出（留给 systemd 的 StartLimit 收敛）；
// 一个 Bot 被平台永久拒绝（下架/封禁）不该带走另一个：进程继续跑，健康那个
// 照常收消息、照常回复，Fatal 也不能报——只有"整机一条连接都不剩"才退。
//
// 这条覆盖「单个 Bot 连接失败不阻塞其他 Bot」里最难的一支：装配全都成功、
// 连接也建立了，是**运行期**才死掉一个。failing 态与退避是 #63，这里只要求
// 不被连坐。
func TestOneDeadBotDoesNotStopTheOthers(t *testing.T) {
	p := newFakePlatform(t)
	p.fatalFor = map[string]int{"102000001": 4915} // 4915 = 机器人已封禁
	cfg := testConfig(t, p, "102000001", "102000002")
	log, logs := testLogger()
	c := startContainer(t, cfg, p, func(o *Options) { o.Log = log })

	p.waitReady(t, "102000002")
	// 先说清楚"死"这件事怎么算数：一条点名 appid 的 Error。
	waitLog(t, logs, "网关永久性错误", "bot=102000001")

	// 健康那个仍然能收能回——这是没被连坐的实质证据。
	p.push(t, "102000002", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-OK", "G-OK", "still-alive"))
	sends := p.waitSends(t, 1)
	if len(sends) != 1 || sends[0].appID != "102000002" || sends[0].content != "echo:still-alive" {
		t.Fatalf("健康 Bot 应照常收发，实际 %+v", sends)
	}

	// 只剩一条活着的连接时还不能退：进程退出等于替一个坏租户停掉其他租户。
	select {
	case err := <-c.Fatal():
		t.Fatalf("单个 Bot 永久失败不该让进程退出，收到 %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// 单个失败不影响其他 Bot，这条也由 TestLockedBotDoesNotStopTheOthers 覆盖。
func TestAllGatewaysFailingSurfacesFatal(t *testing.T) {
	p := newFakePlatform(t)
	// 每个 Bot 的第一轮连接都收到"机器人已封禁"，网关返回 *FatalError。
	p.closeFirstWS = 4915
	cfg := testConfig(t, p, "102000001")

	c := startContainer(t, cfg, p)
	select {
	case err := <-c.Fatal():
		if err == nil {
			t.Fatal("Fatal 收到 nil")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("全部网关都永久失败时应向调用方报致命错误")
	}
}

// 启动汇总必须是一行、带 Bot 数量与耗时：错峰代价要能一眼看出来。
func TestStartupSummaryIsVisible(t *testing.T) {
	p := newFakePlatform(t)
	cfg := testConfig(t, p, "102000001", "102000002")
	log, logs := testLogger()
	c := startContainer(t, cfg, p, func(o *Options) { o.Log = log })
	_ = c

	out := logs()
	for _, want := range []string{"Bot 启动完成", "bots=2", "started=2", "elapsed=", "plugins_total=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("启动日志缺少 %q：\n%s", want, out)
		}
	}
	// 每个 Bot 各自一行，带库路径与插件名（secret 永不出现）。
	for _, want := range []string{"appid=102000001", "appid=102000002", "plugin_names=echo"} {
		if !strings.Contains(out, want) {
			t.Errorf("启动日志缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret-102000001") {
		t.Errorf("启动日志泄露了 secret：\n%s", out)
	}
}

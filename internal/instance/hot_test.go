package instance

// 单 Bot 热增删改的验收（#68）。
//
// 这一组用例全部走**真容器 + 真配置文件 + 假平台**：热重载的价值就在"重读
// 磁盘 → 按差集动手"，把配置文件换成一个内存结构就测不到最要紧的那一步。
// 断言落在外部可观察的地方：假平台上的连接、库里的行、bot_state 里的状态。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	"github.com/knight000/go_qqbot/pkg/host"
)

// botEntry 造一条 [[bots]] 条目（与 testConfig 同口径）。
func botEntry(appID string) config.Bot {
	return config.Bot{AppID: appID, Secret: "secret-" + appID, CertType: config.CertPersonal}
}

// writeConfig 把配置写成一份真实的 TOML 文件，返回路径与解析结果。
//
// 为什么不用 testConfig：那些用例测的是"按内存里的配置装配"，而热重载的
// 语义是"以磁盘上的文件为准"。只有真写一次文件，才能证明 secret 换掉、
// 条目增删这些事真的被读到了。
func writeConfig(t *testing.T, p *fakePlatform, dir string, mediaDir string, bots ...config.Bot) (string, *config.Config) {
	t.Helper()
	// 环境变量覆盖会污染"文件是唯一事实源"的前提，先清掉。
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_APP_SECRET", "")

	var b strings.Builder
	fmt.Fprintf(&b, "[core]\napi_base = %q\ntoken_url = %q\nowners = [\"owner-openid\"]\n\n",
		p.srv.URL, p.srv.URL+"/token")
	for _, bot := range bots {
		b.WriteString("[[bots]]\n")
		fmt.Fprintf(&b, "appid = %q\nsecret = %q\ncert_type = %q\n", bot.AppID, bot.Secret, bot.CertType)
		if !bot.IsEnabled() {
			b.WriteString("enabled = false\n")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "[bot]\nintents = %d\ngateway_stagger = \"0s\"\n\n", 1<<25)
	fmt.Fprintf(&b, "[store]\npath = %q\n\n", filepath.Join(dir, "qqbot.db"))
	fmt.Fprintf(&b, "[media]\ndir = %q\n\n", mediaDir)
	b.WriteString("[group]\nwhitelist = false\nblacklist = false\n\n")
	b.WriteString("[log]\nlevel = \"error\"\nformat = \"text\"\n")

	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("解析刚写的配置: %v\n%s", err, b.String())
	}
	return path, cfg
}

// startHot 起一个带 ConfigPath 的容器，测试结束自动关停。
func startHot(t *testing.T, cfg *config.Config, path string, p *fakePlatform) *Container {
	t.Helper()
	c, err := Start(context.Background(), Options{
		Config:     cfg,
		ConfigPath: path,
		Log:        testLogOrDiscard(),
		Outbound:   p.httpx(),
		Handshake:  p.srv.Client(),
		LoadPlugins: func(context.Context, *slog.Logger) ([]host.Plugin, error) {
			return []host.Plugin{&stubPlugin{name: "echo"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("启动容器: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = c.Shutdown(ctx)
	})
	return c
}

// connOf 返回假平台上某个 Bot 当前那条连接（nil 表示没有）。
func connOf(p *fakePlatform, appID string) *wsConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[appID]
}

// waitConnChange 等某个 Bot 换掉旧连接（热重连是异步的，不能立刻断言）。
func waitConnChange(t *testing.T, p *fakePlatform, appID string, old *wsConn) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cur := connOf(p, appID); cur != nil && cur != old {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s 没有换到新连接", appID)
}

// countGatewayCalls 返回某个 Bot 取网关地址的次数。
func countGatewayCalls(p *fakePlatform, appID string) int {
	calls, _ := p.gatewaySnapshot()
	n := 0
	for _, c := range calls {
		if c.appID == appID {
			n++
		}
	}
	return n
}

// 新增一个条目：/bot reload 即上线，其余 Bot 的连接不中断。
//
// 这是本票最核心的验收。断言"没有中断"用两件证据：那两条连接的对象没换
// （重连会换 wsConn）、且它们取网关地址的次数没增加（重连必然多取一次）。
func TestReloadAddsBotWithoutTouchingOthers(t *testing.T) {
	isolation := t.TempDir()
	isolateTemp(t, isolation)
	p := newFakePlatform(t)
	dir := t.TempDir()

	path, cfg := writeConfig(t, p, dir, filepath.Join(dir, "media"), botEntry("102000001"), botEntry("102000002"))
	seedActiveBots(t, cfg)
	// 第三个 Bot 的库先预置成已认领：按 ADR-0008 决策 8，新 Bot 是
	// pending_claim，认领之前闸门会丢掉每一条普通事件——那是对的，但会让
	// "热新增之后能不能收发"这件事测不出来。这里验的是"上线即可用"。
	seedActiveBots(t, &config.Config{Store: cfg.Store, Bots: []config.Bot{botEntry("102000003")}})
	c := startHot(t, cfg, path, p)

	p.waitReady(t, "102000001")
	p.waitReady(t, "102000002")
	aConn, bConn := connOf(p, "102000001"), connOf(p, "102000002")
	if aConn == nil || bConn == nil {
		t.Fatal("两个 Bot 都应有活着的连接")
	}

	// 配置文件里加第三个 Bot，再让容器重读。
	writeConfig(t, p, dir, filepath.Join(dir, "media"),
		botEntry("102000001"), botEntry("102000002"), botEntry("102000003"))
	sum, err := c.ReloadAll(context.Background(), "102000001")
	if err != nil {
		t.Fatalf("热重载: %v", err)
	}
	if sum.Added != 1 || sum.Restarted != 0 || sum.Stopped != 0 {
		t.Fatalf("差集应为 新增 1、重启 0、停用 0，实际 %+v", sum)
	}

	p.waitReady(t, "102000003")
	if got := connOf(p, "102000001"); got != aConn {
		t.Fatal("新增条目不该换掉 A 的连接")
	}
	if got := connOf(p, "102000002"); got != bConn {
		t.Fatal("新增条目不该换掉 B 的连接")
	}
	if n := countGatewayCalls(p, "102000001"); n != 1 {
		t.Fatalf("A 只该取过一次网关地址，实际 %d 次（说明它被重连了）", n)
	}
	if n := countGatewayCalls(p, "102000002"); n != 1 {
		t.Fatalf("B 只该取过一次网关地址，实际 %d 次", n)
	}

	// 事件连续性：A 在重载之后照常收发。
	p.push(t, "102000001", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-AFTER", "G-A", "still-here"))
	got := p.waitSends(t, 1)
	if got[0].appID != "102000001" || got[0].content != "echo:still-here" {
		t.Fatalf("A 在重载后应照常回复，实际 %+v", got)
	}
	// 新上线的 C 也能收能回。
	p.push(t, "102000003", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-C", "G-C", "hello-C"))
	sends := p.waitSends(t, 2)
	if sends[1].appID != "102000003" || sends[1].content != "echo:hello-C" {
		t.Fatalf("新 Bot 应立刻可用，实际 %+v", sends)
	}
}

// 删掉一个条目：该 Bot 停止、数据保留、其余不受影响；再加回来数据还在。
func TestReloadRemovesBotKeepingData(t *testing.T) {
	isolation := t.TempDir()
	isolateTemp(t, isolation)
	p := newFakePlatform(t)
	dir := t.TempDir()
	media := filepath.Join(dir, "media")

	path, cfg := writeConfig(t, p, dir, media, botEntry("102000001"), botEntry("102000002"))
	seedActiveBots(t, cfg)
	c := startHot(t, cfg, path, p)
	p.waitReady(t, "102000001")
	p.waitReady(t, "102000002")

	// 先让 B 留下一条消息、一个群与一个管理员：这三样都必须活过热删。
	ctx := context.Background()
	bRT := c.findRuntime("102000002")
	if bRT == nil || bRT.db == nil {
		t.Fatal("B 应在运行")
	}
	if err := bRT.db.InsertMessage(ctx, messageRow("EV-KEEP", "G-KEEP")); err != nil {
		t.Fatalf("写一条历史消息: %v", err)
	}
	seedGroup(t, bRT.db, "G-KEEP")
	if err := bRT.db.UpsertAdmin(ctx, adminRow("owner-b")); err != nil {
		t.Fatalf("写管理员: %v", err)
	}
	aConn := connOf(p, "102000001")

	// 从配置里删掉 B。
	path, cfg = writeConfig(t, p, dir, media, botEntry("102000001"))
	sum, err := c.ReloadAll(context.Background(), "102000001")
	if err != nil {
		t.Fatalf("热重载: %v", err)
	}
	if sum.Stopped != 1 || sum.Added != 0 || sum.Restarted != 0 {
		t.Fatalf("差集应为 停用 1，实际 %+v", sum)
	}
	if c.findRuntime("102000002") != nil {
		t.Fatal("B 应已从运行态摘掉")
	}
	if got := connOf(p, "102000001"); got != aConn {
		t.Fatal("删掉 B 不该影响 A 的连接")
	}

	// 数据保留：库还在，行还在，状态是 suspended 且原因是 config_gone。
	db := openStoreForTest(t, cfg.BotStorePath(botEntry("102000002")))
	defer func() { _ = db.Close() }()
	if rows, err := db.QueryRows(ctx, `SELECT event_id FROM messages WHERE event_id = 'EV-KEEP'`); err != nil || len(rows) != 1 {
		t.Fatalf("历史消息应保留: rows=%v err=%v", rows, err)
	}
	if _, ok, err := db.GetGroup(ctx, "G-KEEP"); err != nil || !ok {
		t.Fatalf("群名单应保留: ok=%v err=%v", ok, err)
	}
	if n, err := db.CountAdmins(ctx); err != nil || n != 1 {
		t.Fatalf("管理员应保留: n=%d err=%v", n, err)
	}
	if st, err := core.LoadBotState(ctx, db); err != nil || st != core.BotStateSuspended {
		t.Fatalf("状态应为 suspended，实际 %q err=%v", st, err)
	}
	if reason, err := core.LoadSuspendReason(ctx, db); err != nil || reason != core.SuspendReasonConfigGone {
		t.Fatalf("停用原因应为 config_gone，实际 %q err=%v", reason, err)
	}

	// 再加回去：群名单与历史都还在，且状态回到 active（admins 非空）。
	writeConfig(t, p, dir, media, botEntry("102000001"), botEntry("102000002"))
	sum, err = c.ReloadAll(context.Background(), "102000001")
	if err != nil {
		t.Fatalf("再加回来: %v", err)
	}
	if sum.Added != 1 {
		t.Fatalf("应重新装上一个 Bot，实际 %+v", sum)
	}
	db2 := openStoreForTest(t, cfg.BotStorePath(botEntry("102000002")))
	defer func() { _ = db2.Close() }()
	if st, err := core.LoadBotState(ctx, db2); err != nil || st != core.BotStateActive {
		t.Fatalf("加回来之后应为 active，实际 %q err=%v", st, err)
	}
	if _, ok, err := db2.GetGroup(ctx, "G-KEEP"); err != nil || !ok {
		t.Fatalf("加回来之后群名单应还在: ok=%v err=%v", ok, err)
	}
	if rows, _ := db2.QueryRows(ctx, `SELECT event_id FROM messages WHERE event_id = 'EV-KEEP'`); len(rows) != 1 {
		t.Fatalf("加回来之后历史应还在: %v", rows)
	}
}

// /bot reload <appid> 只影响这一个：其余 Bot 的连接与令牌缓存不变。
func TestReloadOneOnlyTouchesThatBot(t *testing.T) {
	isolation := t.TempDir()
	isolateTemp(t, isolation)
	p := newFakePlatform(t)
	dir := t.TempDir()

	path, cfg := writeConfig(t, p, dir, filepath.Join(dir, "media"), botEntry("102000001"), botEntry("102000002"))
	seedActiveBots(t, cfg)
	c := startHot(t, cfg, path, p)
	p.waitReady(t, "102000001")
	p.waitReady(t, "102000002")

	aConn, bConn := connOf(p, "102000001"), connOf(p, "102000002")
	aRT := c.findRuntime("102000001")
	ctx := context.Background()
	tokenBefore, _, err := aRT.db.GetState(ctx, "access_token")
	if err != nil || tokenBefore == "" {
		t.Fatalf("A 的令牌应已缓存: %q err=%v", tokenBefore, err)
	}

	if err := c.ReloadOne(ctx, "102000001", "102000002"); err != nil {
		t.Fatalf("单 Bot 重连: %v", err)
	}
	// 重连是异步的：等它真的换了一条连接（旧连接对象不能被原地复用）。
	waitConnChange(t, p, "102000002", bConn)
	if connOf(p, "102000001") != aConn {
		t.Fatal("其余 Bot 的连接不该被动")
	}

	// 令牌缓存不变：重连不该让别人重新取一次 token。
	db := openStoreForTest(t, cfg.BotStorePath(botEntry("102000001")))
	defer func() { _ = db.Close() }()
	if token, _, err := db.GetState(ctx, "access_token"); err != nil || token != tokenBefore {
		t.Fatalf("A 的令牌缓存不该变: %q（原 %q）err=%v", token, tokenBefore, err)
	}
	if n := countGatewayCalls(p, "102000001"); n != 1 {
		t.Fatalf("A 不该被重连（取地址 %d 次）", n)
	}
}

// 重连自己会被明确拒绝，而不是把这条回复弄丢。
func TestReloadOneRefusesSelf(t *testing.T) {
	isolation := t.TempDir()
	isolateTemp(t, isolation)
	p := newFakePlatform(t)
	dir := t.TempDir()

	path, cfg := writeConfig(t, p, dir, filepath.Join(dir, "media"), botEntry("102000001"))
	seedActiveBots(t, cfg)
	c := startHot(t, cfg, path, p)
	p.waitReady(t, "102000001")

	if err := c.ReloadOne(context.Background(), "102000001", "102000001"); err != core.ErrBotSelfOp {
		t.Fatalf("应拒绝重连自己，实际 %v", err)
	}
}

// purge 的三条约束：配置里还在时拒绝、先删文件再删行、保留群名单与管理员。
func TestPurgeRequiresConfigGoneAndClearsData(t *testing.T) {
	isolation := t.TempDir()
	isolateTemp(t, isolation)
	p := newFakePlatform(t)
	dir := t.TempDir()
	mediaDir := filepath.Join(dir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	path, cfg := writeConfig(t, p, dir, mediaDir, botEntry("102000001"), botEntry("102000002"))
	seedActiveBots(t, cfg)
	c := startHot(t, cfg, path, p)
	p.waitReady(t, "102000001")
	ctx := context.Background()

	// 配置里还在：必须拒绝，否则清完库下一次 reload 会把它拉起来。
	if _, err := c.PurgeStats(ctx, "102000002"); err != core.ErrBotStillConfigured {
		t.Fatalf("配置里还在时应返回 ErrBotStillConfigured，实际 %v", err)
	}
	if _, _, err := c.Purge(ctx, "102000002"); err != core.ErrBotStillConfigured {
		t.Fatalf("Purge 本身也必须再判一次，实际 %v", err)
	}

	// 准备数据：一条消息 + 一个媒体文件 + 一个管理员 + 一个群。
	db := openStoreForTest(t, cfg.BotStorePath(botEntry("102000002")))
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertMessage(ctx, messageRow("EV-PURGE", "G-PURGE")); err != nil {
		t.Fatal(err)
	}
	seedGroup(t, db, "G-PURGE")
	if err := db.UpsertAdmin(ctx, adminRow("owner-b")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(mediaDir, "pic.png")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	insertMediaRow(t, cfg.BotStorePath(botEntry("102000002")), file)
	_ = db.Close()

	// 删掉配置条目并 reload → 它停用、数据保留。
	writeConfig(t, p, dir, mediaDir, botEntry("102000001"))
	if _, err := c.ReloadAll(ctx, "102000001"); err != nil {
		t.Fatalf("热重载: %v", err)
	}

	stats, err := c.PurgeStats(ctx, "102000002")
	if err != nil {
		t.Fatalf("预览: %v", err)
	}
	if stats.Messages != 1 || stats.MediaRows != 1 {
		t.Fatalf("预览应看到 1 条消息 1 个媒体，实际 %+v", stats)
	}

	counts, files, err := c.Purge(ctx, "102000002")
	if err != nil {
		t.Fatalf("清理: %v", err)
	}
	if counts.Messages != 1 || files != 1 {
		t.Fatalf("应删掉 1 条消息 1 个文件，实际 %+v/%d", counts, files)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("媒体文件应已被删除: %v", err)
	}

	// 保留的东西：管理员、群名单、库本身。
	ver := openStoreForTest(t, cfg.BotStorePath(botEntry("102000002")))
	defer func() { _ = ver.Close() }()
	if rows, _ := ver.QueryRows(ctx, `SELECT event_id FROM messages`); len(rows) != 0 {
		t.Fatalf("消息应被清空: %v", rows)
	}
	if n, err := ver.CountAdmins(ctx); err != nil || n != 1 {
		t.Fatalf("管理员必须保留: n=%d err=%v", n, err)
	}
	if _, ok, err := ver.GetGroup(ctx, "G-PURGE"); err != nil || !ok {
		t.Fatalf("群名单必须保留: ok=%v err=%v", ok, err)
	}
}

// 热删一个 Bot 期间并发推事件：不 panic、不串库、不写进别人的库。
//
// 这是并发验收：删的是 C，事件同时打在 A（活着）与 C（正在被拆）上。
// 要求只有两条，但它们就是"没有竞态"的全部含义——A 的库只出现 A 的事件，
// 且进程没有因为任何一条迟到的写而炸掉。
func TestRemovingBotDuringConcurrentEventsDoesNotCrossStores(t *testing.T) {
	isolation := t.TempDir()
	isolateTemp(t, isolation)
	p := newFakePlatform(t)
	dir := t.TempDir()
	media := filepath.Join(dir, "media")

	path, cfg := writeConfig(t, p, dir, media, botEntry("102000001"), botEntry("102000002"), botEntry("102000003"))
	seedActiveBots(t, cfg)
	c := startHot(t, cfg, path, p)
	for _, id := range []string{"102000001", "102000002", "102000003"} {
		p.waitReady(t, id)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 16 个并发推送者持续往 C（正在被拆的那个）打事件；另有一个慢慢往 A
	// 打，用来证明"拆 C 的同时 A 的事件照常进 A 的库"。
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				ev := fmt.Sprintf("EV-102000003-%d-%d", i, n)
				if err := p.tryPush("102000003", "GROUP_AT_MESSAGE_CREATE", groupMessage(ev, "G-102000003", "hi")); err != nil {
					// 连接被拆时写失败是预期内的：这条事件被丢弃，不是串库。
					return
				}
				time.Sleep(time.Millisecond)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			ev := fmt.Sprintf("EV-102000001-0-%d", n)
			if err := p.tryPush("102000001", "GROUP_AT_MESSAGE_CREATE", groupMessage(ev, "G-102000001", "hi")); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	time.Sleep(80 * time.Millisecond)
	// 从配置里删掉 C 并热重载：拆的这一刻正好有 16 个 goroutine 在往它推。
	writeConfig(t, p, dir, media, botEntry("102000001"), botEntry("102000002"))
	if _, err := c.ReloadAll(ctx, "102000001"); err != nil {
		t.Fatalf("热重载: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	close(stop)
	wg.Wait()
	// 让被打断的那批事件（已入队或正在处理）落定：断言"没写进别的库"
	// 必须在它们都结束之后才成立。
	time.Sleep(100 * time.Millisecond)

	if c.findRuntime("102000003") != nil {
		t.Fatal("C 应已被拆掉")
	}

	// 串库断言：A 的库里不能出现 C 的事件，C 的库里也不能出现 A 的。
	aDB := openStoreForTest(t, cfg.BotStorePath(botEntry("102000001")))
	defer func() { _ = aDB.Close() }()
	aRows, err := aDB.QueryRows(ctx, `SELECT event_id FROM messages`)
	if err != nil {
		t.Fatalf("查 A 的消息: %v", err)
	}
	if len(aRows) == 0 {
		t.Fatal("A 应在并发期间收到过事件")
	}
	for _, row := range aRows {
		id, _ := row["event_id"].(string)
		if strings.HasPrefix(id, "EV-102000003-") {
			t.Fatalf("A 的库里出现了 C 的事件 %s（跨租户污染）", id)
		}
	}

	cDB := openStoreForTest(t, cfg.BotStorePath(botEntry("102000003")))
	defer func() { _ = cDB.Close() }()
	cRows, err := cDB.QueryRows(ctx, `SELECT event_id FROM messages`)
	if err != nil {
		t.Fatalf("查 C 的消息: %v", err)
	}
	for _, row := range cRows {
		id, _ := row["event_id"].(string)
		if strings.HasPrefix(id, "EV-102000001-") {
			t.Fatalf("C 的库里出现了 A 的事件 %s（跨租户污染）", id)
		}
	}

	// A 仍然可用：拆掉 C 之后它照常收发。
	p.push(t, "102000001", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-A-LAST", "G-102000001", "alive"))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		found := false
		for _, s := range p.sends {
			if s.content == "echo:alive" {
				found = true
			}
		}
		p.mu.Unlock()
		if found {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("拆掉 C 之后 A 应仍然可用")
}

// --- 造数据的小工具（库里只有 store 的少量写入口，媒体行直接走 SQL） ---

func messageRow(eventID, groupID string) store.Message {
	return store.Message{
		EventID: eventID, MsgID: "m-" + eventID, Scene: "group",
		PeerOpenID: groupID, SenderOpenID: "u-" + groupID, MsgType: 0, Content: "hi",
	}
}

func adminRow(openid string) store.Admin {
	return store.Admin{OpenID: openid, Role: store.RoleOwner, SourceScene: "c2c"}
}

// seedGroup 把一个群放进白名单（走 store 的观察 + 置模式两条真实路径）。
func seedGroup(t *testing.T, db *store.DB, openid string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := db.ObserveGroup(ctx, openid); err != nil {
		t.Fatalf("观察群 %s: %v", openid, err)
	}
	if err := db.SetGroupMode(ctx, openid, store.GroupModeAllow); err != nil {
		t.Fatalf("放行群 %s: %v", openid, err)
	}
}

// tryPush 与 push 同形状，但写失败只返回错误、不 fatal：
// 连接正在被拆时写失败是**预期**的（那条事件被丢弃），不能算用例失败。
func (p *fakePlatform) tryPush(appID, typ string, payload any) error {
	p.mu.Lock()
	ws := p.conns[appID]
	p.seq[appID]++
	seq := p.seq[appID]
	p.mu.Unlock()
	if ws == nil {
		return fmt.Errorf("%s 没有活着的连接", appID)
	}
	frame, err := json.Marshal(map[string]any{"op": 0, "s": seq, "t": typ, "d": payload})
	if err != nil {
		return err
	}
	return ws.write(frame)
}

// insertMediaRow 直接写一行 media：store 没有暴露"写媒体行"的入口（生产路径
// 只在出站发图时写），而这条用例要验的正是"行与文件都该被清掉"。
func insertMediaRow(t *testing.T, dbPath, file string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("直连 SQLite: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(
		`INSERT INTO media(sha256, kind, path, bytes, message_id, created_at) VALUES(?, 'image', ?, 1, NULL, ?)`,
		"sha-"+filepath.Base(file), file, time.Now().Unix()); err != nil {
		t.Fatalf("写媒体行: %v", err)
	}
}

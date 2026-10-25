package upgrade

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
)

// probeTimeoutForTest 与本仓其它"要起真进程"的用例同一口径：包默认 2 秒，
// 但并行跑测试时冷启一个 5 MB 的产物会超，所以测试给 10 秒。
const probeTimeoutForTest = 10 * time.Second

// fakeReplacer 是插件进程代理的替身：只记录"被要求换哪个文件"，并在那一刻
// 把落位文件读下来——用来证明换上去的正是刚校验过的那份字节。
type fakeReplacer struct {
	bin   string
	err   error
	calls []string
	got   []byte
}

func (f *fakeReplacer) BinPath() string { return f.bin }

func (f *fakeReplacer) ReplaceBinary(newPath string) error {
	f.calls = append(f.calls, newPath)
	f.got, _ = os.ReadFile(newPath)
	return f.err
}

// pluginTarget 造一个"已加载 0.1.0"的插件与它的替身。
func pluginTarget(t *testing.T, name string) (*fakeReplacer, PluginTarget) {
	t.Helper()
	dir := t.TempDir()
	bin := fakePluginBin(t, dir, name, "0.1.0")
	r := &fakeReplacer{bin: bin}
	return r, PluginTarget{Name: name, Version: "0.1.0", Replace: r}
}

// 验收 1（升级侧）：索引里版本更高 → 下载、校验、落位、探测身份、交给
// ReplaceBinary。落位文件必须与插件同目录（同卷才谈得上 rename）。
func TestUpgradePluginsReplacesHigherVersion(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	blob := fakePluginAsset(t, "echo", "0.2.0")
	f.addPlugin("echo", "0.2.0", blob)

	r, tgt := pluginTarget(t, "echo")
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{tgt})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("结论应有 1 条: %+v", rep.Results)
	}
	got := rep.Results[0]
	if got.Action != "升" {
		t.Fatalf("应判为升，实际 %q（%s）", got.Action, rep.Text())
	}
	if got.From != "0.1.0" || got.To != "0.2.0" {
		t.Errorf("版本 diff 不对: %+v", got)
	}
	if got.Size != int64(len(blob)) {
		t.Errorf("产物大小 = %d want %d", got.Size, len(blob))
	}
	if len(r.calls) != 1 {
		t.Fatalf("应调用一次 ReplaceBinary，实际 %d", len(r.calls))
	}
	if dir := filepath.Dir(r.calls[0]); dir != filepath.Dir(r.bin) {
		t.Errorf("落位文件必须与插件同目录：%s vs %s", dir, filepath.Dir(r.bin))
	}
	if !bytes.Equal(r.got, blob) {
		t.Error("替换用的字节与发布里的产物不一致")
	}
	// 落位文件是点开头的：目录里非点开头的普通文件会被装配期当成插件。
	if base := filepath.Base(r.calls[0]); !strings.HasPrefix(base, ".") {
		t.Errorf("落位文件名 %q 应以点开头", base)
	}
	if f.assetHits.Load() == 0 {
		t.Error("插件产物应走二进制通道")
	}
	for _, want := range []string{"插件升级", "0.1.0 → 0.2.0", "升", "耗时", humanSize(int64(len(blob)))} {
		if !strings.Contains(rep.Text(), want) {
			t.Errorf("报告缺少 %q:\n%s", want, rep.Text())
		}
	}
}

// 验收 3：索引里版本不更高 → 跳过，并且**不下载**。
//
// 两种情形各一条：同版本（已是最新）、本地更高（只升不降）。后者是这条
// 规则真正的意义——不能把用户单独升过的插件静默降级。
func TestUpgradePluginsSkipsWhenNotNewer(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	f.addPlugin("echo", "0.2.0", fakePluginAsset(t, "echo", "0.2.0"))

	cases := []struct {
		local  string
		reason string
	}{
		{"0.2.0", "跳过（已是最新）"},
		{"0.3.0", "跳过（比发布版本新）"},
	}
	for _, c := range cases {
		t.Run(c.local, func(t *testing.T) {
			r := &fakeReplacer{bin: fakePluginBin(t, t.TempDir(), "echo", c.local)}
			u, _ := newTestUpgrader(t, f, "1.2.3", nil)

			rep, err := u.UpgradePlugins(context.Background(),
				[]PluginTarget{{Name: "echo", Version: c.local, Replace: r}})
			if err != nil {
				t.Fatalf("UpgradePlugins: %v", err)
			}
			if a := rep.Results[0].Action; a != c.reason {
				t.Errorf("应跳过并写明原因 %q，实际 %q", c.reason, a)
			}
			if len(r.calls) != 0 {
				t.Errorf("跳过时不该换任何文件，实际 %d 次", len(r.calls))
			}
			if n := f.assetHits.Load(); n != 0 {
				t.Errorf("跳过时不该下载，实际命中 %d 次", n)
			}
			if !strings.Contains(rep.Text(), c.reason) {
				t.Errorf("报告应写明跳过原因 %q://n%s", c.reason, rep.Text())
			}
		})
	}
}

// 索引里没有这个插件、它自己也没声明上游 → 跳过并说明**没有来源**。
//
// #53 之后"索引里没有"不再必然意味着"核心仓发漏了"：外部插件本来就不在核心
// 索引里，它缺的是 Manifest.Upstream。说"发布里没有"会把原因指错方向。
// 这里最容易犯的错是"当成已是最新"，那样 owner 会以为发布里带着它。
func TestUpgradePluginsSkipsMissingFromIndex(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	f.index = `{"schema":1,"core":{"version":"1.2.4"},"plugins":[]}`

	r, tgt := pluginTarget(t, "echo")
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{tgt})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	if a := rep.Results[0].Action; a != "跳过（没有声明发布来源）" {
		t.Fatalf("应说明没有发布来源，实际 %q", a)
	}
	if len(r.calls) != 0 || f.assetHits.Load() != 0 {
		t.Error("没有发布来源时既不该下载也不该替换")
	}
}

// 验收 5：产物自称的版本与索引不一致 → 拒绝替换，旧文件一个字节都不动。
//
// sha256 只能证明"字节没被改过"，证明不了"索引里的版本号与这个产物对得上"。
// 装错产物的现场是"升完版本号没变"，而那时已经换完了。
func TestUpgradePluginsRejectsVersionMismatch(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	blob := fakePluginAsset(t, "echo", "0.1.9") // 产物自称 0.1.9
	f.addPlugin("echo", "0.2.0", blob)          // 索引说 0.2.0

	r, tgt := pluginTarget(t, "echo")
	before := readFile(t, r.bin)
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{tgt})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	got := rep.Results[0]
	if !strings.HasPrefix(got.Action, "失败") || !strings.Contains(got.Action, "0.1.9") {
		t.Fatalf("应因版本不一致失败并写出产物自称的版本，实际 %q", got.Action)
	}
	if len(r.calls) != 0 {
		t.Error("版本不一致时不该换文件")
	}
	if after := readFile(t, r.bin); after != before {
		t.Error("旧文件被改动了")
	}
	if _, err := os.Stat(PluginStagedPath(r.bin)); !os.IsNotExist(err) {
		t.Error("被拒绝的产物不该留在落位路径上")
	}
}

// 同一个坑的另一面：产物自称是别的插件。装上去等于把路由指给一个不存在
// 的插件，所以必须在下载之后、换文件之前拒绝。
func TestUpgradePluginsRejectsNameMismatch(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	f.addPlugin("echo", "0.2.0", fakePluginAsset(t, "other", "0.2.0"))

	r, tgt := pluginTarget(t, "echo")
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{tgt})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	got := rep.Results[0]
	if !strings.HasPrefix(got.Action, "失败") || !strings.Contains(got.Action, "other") {
		t.Fatalf("应因名字不一致失败，实际 %q", got.Action)
	}
	if len(r.calls) != 0 {
		t.Error("名字不一致时不该换文件")
	}
}

// 产物连 `--manifest` 都答不上来（不是插件、或起不来）→ 拒绝，旧文件不动。
func TestUpgradePluginsRejectsUnprobableAsset(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	// 一份能通过文件头检查、但不会应答清单的内容：测试二进制本身（没有
	// 注入清单时它会当普通测试进程跑，收到 --manifest 直接报错退出）。
	selfPath, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制: %v", err)
	}
	self, err := os.ReadFile(selfPath)
	if err != nil {
		t.Fatalf("读测试二进制: %v", err)
	}
	f.addPlugin("echo", "0.2.0", self)

	r, tgt := pluginTarget(t, "echo")
	before := readFile(t, r.bin)
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{tgt})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	got := rep.Results[0]
	if !strings.HasPrefix(got.Action, "失败") || !strings.Contains(got.Action, "--manifest") {
		t.Fatalf("应因探测失败而拒绝，实际 %q", got.Action)
	}
	if len(r.calls) != 0 {
		t.Error("探测失败时不该换文件")
	}
	if readFile(t, r.bin) != before {
		t.Error("旧文件被改动了")
	}
	if _, err := os.Stat(PluginStagedPath(r.bin)); !os.IsNotExist(err) {
		t.Error("探测失败的产物不该留在落位路径上")
	}
}

// 验收 2（升级侧）：换进程失败（新版本握手不过）→ 结论写"已回滚"，
// 并把回滚后的版本一起说出来，别让 owner 以为插件现在是坏的。
func TestUpgradePluginsReportsRollback(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	f.addPlugin("echo", "0.2.0", fakePluginAsset(t, "echo", "0.2.0"))

	r, tgt := pluginTarget(t, "echo")
	r.err = errors.New("新版本 5s 内没有完成握手")
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{tgt})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	got := rep.Results[0]
	for _, want := range []string{"失败", "已回滚到 0.1.0", "没有完成握手"} {
		if !strings.Contains(got.Action, want) {
			t.Errorf("结论缺少 %q：%q", want, got.Action)
		}
	}
	if rep.Changed() != 0 {
		t.Error("回滚不算升级成功")
	}
}

// 插件升级只读 index.json：核心产物的 checksums.txt 与它无关。// 用一个空的 checksums 断言这条路不碰核心清单。
func TestUpgradePluginsNeedsNoCoreChecksums(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	f.sums = ""
	f.addPlugin("echo", "0.2.0", fakePluginAsset(t, "echo", "0.2.0"))

	// 反证：核心升级这时候必须失败（它依赖 checksums）。
	core, _ := newTestUpgrader(t, f, "1.2.3", nil)
	if _, err := core.Apply(context.Background()); err == nil {
		t.Fatal("没有 checksums 时核心升级应失败（否则下面的断言就验不到什么）")
	}

	r, tgt := pluginTarget(t, "echo")
	plugins, _ := newTestUpgrader(t, f, "1.2.3", nil)
	rep, err := plugins.UpgradePlugins(context.Background(), []PluginTarget{tgt})
	if err != nil {
		t.Fatalf("插件升级不该依赖核心清单: %v", err)
	}
	if rep.Results[0].Action != "升" {
		t.Fatalf("应升，实际 %q", rep.Results[0].Action)
	}
	if len(r.calls) != 1 {
		t.Error("应调用一次替换")
	}
}

// 关掉自更新 / dev 版本：一个请求都不发（与核心升级同一口径）。
func TestUpgradePluginsRefusesDisabledAndDev(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	f.addPlugin("echo", "0.2.0", fakePluginAsset(t, "echo", "0.2.0"))
	f.broken.Store(true)

	_, tgt := pluginTarget(t, "echo")
	off, _ := newTestUpgrader(t, f, "1.2.3", func(o *Options) { o.Enabled = false })
	if _, err := off.UpgradePlugins(context.Background(), []PluginTarget{tgt}); err != ErrDisabled {
		t.Fatalf("关掉自更新应返回 ErrDisabled，实际: %v", err)
	}
	dev, _ := newTestUpgrader(t, f, "dev", nil)
	if _, err := dev.UpgradePlugins(context.Background(), []PluginTarget{tgt}); err == nil {
		t.Fatal("dev 版本应拒绝")
	}
	if n := f.apiHits.Load(); n != 0 {
		t.Errorf("拒绝时不该发任何请求，实际 %d 次", n)
	}
}

// 验收 4：核心升级同时带新版插件——只替换版本更高的，低的不被降级。
//
// Apply 只负责"落位到 .new"（换盘要等关停之后），Commit 在 main 里按
// rename 规则落位。两步分开断言：只验其中一步会漏掉"落位了但没换"或
// "换了但换错文件"。
func TestApplyStagesOnlyHigherPluginsAndCommitLandsThem(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	echoBlob := fakePluginAsset(t, "echo", "0.2.0")
	f.addPlugin("echo", "0.2.0", echoBlob)
	f.addPlugin("md", "0.1.0", fakePluginAsset(t, "md", "0.1.0")) // 本地更高，应跳过

	pluginDir := t.TempDir()
	echoBin := fakePluginBin(t, pluginDir, "echo", "0.1.0")
	mdBin := fakePluginBin(t, pluginDir, "md", "0.2.0")
	mdBefore := readFile(t, mdBin)

	u, _ := newTestUpgrader(t, f, "1.2.3", func(o *Options) {
		o.Plugins = []LoadedPlugin{
			{Name: "echo", Version: "0.1.0", Bin: echoBin},
			{Name: "md", Version: "0.2.0", Bin: mdBin},
		}
	})

	plan, err := u.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	byName := map[string]PluginDelta{}
	for _, d := range plan.Plugins {
		byName[d.Name] = d
	}
	if got := byName["echo"]; got.Action != "升" || got.Size != int64(len(echoBlob)) {
		t.Errorf("echo 应升并带上产物大小，实际 %+v", got)
	}
	if got := byName["md"].Action; got != "跳过（比发布版本新）" {
		t.Errorf("md 应跳过，实际 %q", got)
	}
	if len(plan.PluginStages) != 1 || plan.PluginStages[0].Name != "echo" {
		t.Fatalf("只该落位 echo: %+v", plan.PluginStages)
	}
	// 关停之前不能动正式文件：这一步还没到"进程已退出"的时候。
	if got := readFile(t, echoBin); got == string(echoBlob) {
		t.Error("Apply 阶段不该直接替换插件")
	}
	if _, err := os.Stat(PluginStagedPath(echoBin)); err != nil {
		t.Errorf("echo 的新产物应已落位: %v", err)
	}
	if _, err := os.Stat(PluginStagedPath(mdBin)); !os.IsNotExist(err) {
		t.Error("md（本地更高）不该落位任何产物")
	}

	if err := Commit(plan, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := readFile(t, echoBin); got != string(echoBlob) {
		t.Error("Commit 之后 echo 应是新产物")
	}
	if got := readFile(t, mdBin); got != mdBefore {
		t.Error("md 被降级了（只升不降）")
	}
	if _, err := os.Stat(PluginStagedPath(echoBin)); !os.IsNotExist(err) {
		t.Error("落位文件应已被 rename 走")
	}
	if _, err := os.Stat(pluginOldPath(echoBin)); !os.IsNotExist(err) {
		t.Error("成功之后不该留下旧版本备份")
	}
}

// 插件落位失败不拦核心升级：核心是这次的主诉求，一个插件资产发漏了不该
// 让它一起作废。失败原因必须进报告——否则 owner 以为插件也跟着升了。
func TestApplyKeepsCoreUpgradeWhenPluginAssetFails(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	// 索引里有 echo 0.2.0，但镜像通道没有这份产物 → 404。
	f.addPlugin("echo", "0.2.0", fakePluginAsset(t, "echo", "0.2.0"))
	delete(f.pluginBlobs, f.pluginAssetNames()[0])

	pluginDir := t.TempDir()
	echoBin := fakePluginBin(t, pluginDir, "echo", "0.1.0")
	u, _ := newTestUpgrader(t, f, "1.2.3", func(o *Options) {
		o.Plugins = []LoadedPlugin{{Name: "echo", Version: "0.1.0", Bin: echoBin}}
	})

	plan, err := u.Apply(context.Background())
	if err != nil {
		t.Fatalf("插件失败不该拦核心升级: %v", err)
	}
	if plan.To != "1.2.4" {
		t.Errorf("核心仍应升到 1.2.4，实际 %q", plan.To)
	}
	d := plan.Plugins[0]
	if !strings.HasPrefix(d.Action, "失败") {
		t.Fatalf("插件应标为失败，实际 %q", d.Action)
	}
	if len(plan.PluginStages) != 0 {
		t.Error("失败的插件不该进落位清单")
	}
}

// all 的顺序由 index.json 决定：两次报告要能逐行比对，顺序不能随输入漂移。
func TestUpgradePluginsOrdersByIndex(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	f.addPlugin("alpha", "0.2.0", fakePluginAsset(t, "alpha", "0.2.0"))
	f.addPlugin("zeta", "0.2.0", fakePluginAsset(t, "zeta", "0.2.0"))

	_, a := pluginTarget(t, "alpha")
	_, z := pluginTarget(t, "zeta")
	// 故意反着传：报告顺序应当由索引决定，不是入参顺序。
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)
	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{z, a})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	if rep.Results[0].Name != "alpha" || rep.Results[1].Name != "zeta" {
		t.Fatalf("顺序应按索引: %+v", rep.Results)
	}
}

// 没给定落位路径（插件不是子进程、或装配期拿不到路径）时，插件只能跳过：
// 猜一个路径去下载，换上去的可能是另一份产物。
func TestUpgradePluginsSkipsWithoutBinPath(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	f.addPlugin("echo", "0.2.0", fakePluginAsset(t, "echo", "0.2.0"))

	u, _ := newTestUpgrader(t, f, "1.2.3", nil)
	rep, err := u.UpgradePlugins(context.Background(),
		[]PluginTarget{{Name: "echo", Version: "0.1.0", Replace: &fakeReplacer{}}})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	if a := rep.Results[0].Action; !strings.HasPrefix(a, "跳过") || !strings.Contains(a, "路径") {
		t.Fatalf("没有路径时应跳过并说明，实际 %q", a)
	}
	if f.assetHits.Load() != 0 {
		t.Error("没有落位路径时不该下载")
	}
}

// host.Manifest 是探测的产物；这条断言只是把"测试夹具真的编出了合法清单"
// 这件事钉住——否则上面所有"探测通过"的用例都可能是在验一个空壳。
func TestFakePluginAssetAnswersManifest(t *testing.T) {
	dir := t.TempDir()
	bin := fakePluginBin(t, dir, "echo", "0.1.0")
	m, err := probeManifest(context.Background(), bin, probeTimeoutForTest)
	if err != nil {
		t.Fatalf("探测: %v", err)
	}
	if m.Name != "echo" || m.Version != "0.1.0" || m.APIVersion != host.APIVersion {
		t.Fatalf("清单不对: %+v", m)
	}
}

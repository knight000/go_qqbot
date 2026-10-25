package upgrade

// #53：外部插件自带发布源（Manifest.Upstream）。一个插件只有一个来源——
// 声明了上游就只查上游，查不到就是"跳过"，绝不回落到核心索引。
//
// 这里钉住的是**结论文案**（失败分类表逐字）与"上游用自己那个仓"这两件事：
// 后者只有跨仓的假 API 能验——同一个 tag 名在两个仓含义不同。

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// 验收：上游有新版本 → 下载、校验、落位、验身份、换进程。
//
// 上游 tag 故意与核心仓同名（都叫 v1.2.4）：如果实现拿核心仓的 client 去查
// 上游，或者拿核心仓的 tag 缓存命中上游的查询，这里会取不到产物。
func TestUpgradePluginsFromUpstreamReplacesHigherVersion(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	up := f.addUpstream("o/up", "v1.2.4")
	blob := fakePluginAsset(t, "torn", "0.2.0")
	up.addPlugin("torn", "0.2.0", blob, true)

	r := &fakeReplacer{bin: fakePluginBin(t, t.TempDir(), "torn", "0.1.0")}
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{
		{Name: "torn", Version: "0.1.0", Upstream: "o/up", Replace: r},
	})
	if err != nil {
		t.Fatalf("上游不可达之外的失败不该让整条命令报错: %v", err)
	}
	got := rep.Results[0]
	if got.Action != "升" {
		t.Fatalf("应判为升，实际 %q（%s）", got.Action, rep.Text())
	}
	if got.From != "0.1.0" || got.To != "0.2.0" {
		t.Errorf("版本 diff 不对: %+v", got)
	}
	if want := "o/up@v1.2.4"; got.Source != want {
		t.Errorf("来源 = %q，期望 %q（上游自己的仓与 tag）", got.Source, want)
	}
	if len(r.calls) != 1 {
		t.Fatalf("应调用一次 ReplaceBinary，实际 %d", len(r.calls))
	}
	if !bytes.Equal(r.got, blob) {
		t.Error("替换用的字节与上游产物不一致")
	}
	// 全部目标都声明了上游时，核心索引一个请求都不该发。
	if n := f.apiHits.Load(); n != 0 {
		t.Errorf("全上游的批次不该查核心索引，实际命中 %d 次", n)
	}
	if n := f.upstreamHits.Load(); n != 1 {
		t.Errorf("应只查一次上游最新发布，实际 %d 次", n)
	}
	// 报告：头部只留平台（混合来源下"发布 <tag>"不再成立），每条带来源。
	text := rep.Text()
	if !strings.HasPrefix(text, "插件升级（"+currentPlatform()+"）") {
		t.Errorf("报告头部应是「插件升级（<平台>）」:\n%s", text)
	}
	if !strings.Contains(text, "（来源 o/up@v1.2.4）") {
		t.Errorf("每条 delta 末尾应带来源:\n%s", text)
	}
}

// 失败分类表第一行：404 / 403 / 429 / 超时 / JSON 坏 → 跳过（无法访问：<一句话>）。
//
// `all` 里一个上游不可达不该让整条命令看起来失败——用户要的是"能升的升了"。
func TestUpgradePluginsSkipsWhenUpstreamUnreachable(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeUpstream)
		want  string
	}{
		{"404", func(u *fakeUpstream) { u.status = 404 }, "跳过（无法访问：404 Not Found）"},
		{"403", func(u *fakeUpstream) { u.status = 403 }, "跳过（无法访问：403 Forbidden）"},
		{"429", func(u *fakeUpstream) { u.status = 429 }, "跳过（无法访问：429 Too Many Requests）"},
		{"500", func(u *fakeUpstream) { u.status = 500 }, "跳过（无法访问：500 Internal Server Error）"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRelease(t, "1.2.4", peHeader("core"))
			up := f.addUpstream("o/up", "v9.9.9")
			up.addPlugin("torn", "0.2.0", fakePluginAsset(t, "torn", "0.2.0"), true)
			c.setup(up)

			r := &fakeReplacer{bin: fakePluginBin(t, t.TempDir(), "torn", "0.1.0")}
			u, _ := newTestUpgrader(t, f, "1.2.3", nil)
			rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{
				{Name: "torn", Version: "0.1.0", Upstream: "o/up", Replace: r},
			})
			if err != nil {
				t.Fatalf("上游不可达不该让整条命令报错: %v", err)
			}
			if got := rep.Results[0].Action; got != c.want {
				t.Errorf("结论 = %q，期望 %q", got, c.want)
			}
			if len(r.calls) != 0 || f.assetHits.Load() != 0 {
				t.Error("不可达时既不该下载也不该替换")
			}
		})
	}
}

// JSON 坏也是"无法访问"，但那一句话来自解析错误：只要求是同一类文案、
// 且不把换行与 URL 塞进报告。
func TestUpgradePluginsSkipsWhenUpstreamJSONBroken(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	up := f.addUpstream("o/up", "v9.9.9")
	up.brokenJSON = true

	_, tgt := pluginTarget(t, "torn")
	tgt.Upstream = "o/up"
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{tgt})
	if err != nil {
		t.Fatalf("上游 JSON 坏不该让整条命令报错: %v", err)
	}
	got := rep.Results[0].Action
	if !strings.HasPrefix(got, "跳过（无法访问：") || !strings.HasSuffix(got, "）") {
		t.Fatalf("结论应是「跳过（无法访问：<一句话>）」，实际 %q", got)
	}
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("一句话里不该有换行: %q", got)
	}
}

// 失败分类表第二行：上游最新版没有本平台产物 → 跳过（上游没有 <os>-<arch> 的产物）。
func TestUpgradePluginsSkipsWhenUpstreamHasNoPlatformAsset(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	up := f.addUpstream("o/up", "v9.9.9")
	// 只有别的平台：名字里的平台段不是本机的。
	up.addRaw("qqbot-plugin-torn-0.2.0-plan9-mips", fakePluginAsset(t, "torn", "0.2.0"), true)

	r := &fakeReplacer{bin: fakePluginBin(t, t.TempDir(), "torn", "0.1.0")}
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)
	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{
		{Name: "torn", Version: "0.1.0", Upstream: "o/up", Replace: r},
	})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	want := "跳过（上游没有 " + currentPlatform() + " 的产物）"
	if got := rep.Results[0].Action; got != want {
		t.Errorf("结论 = %q，期望 %q", got, want)
	}
	if len(r.calls) != 0 || f.assetHits.Load() != 0 {
		t.Error("没有本平台产物时既不该下载也不该替换")
	}
}

// 失败分类表第三行：上游版本 ≤ 磁盘版本 → 跳过（已是最新）/ 跳过（比上游版本新）。
func TestUpgradePluginsSkipsWhenUpstreamNotNewer(t *testing.T) {
	cases := []struct {
		local string
		want  string
	}{
		{"0.2.0", "跳过（已是最新）"},
		{"0.3.0", "跳过（比上游版本新）"},
	}
	for _, c := range cases {
		t.Run(c.local, func(t *testing.T) {
			f := newFakeRelease(t, "1.2.4", peHeader("core"))
			up := f.addUpstream("o/up", "v9.9.9")
			up.addPlugin("torn", "0.2.0", fakePluginAsset(t, "torn", "0.2.0"), true)

			r := &fakeReplacer{bin: fakePluginBin(t, t.TempDir(), "torn", c.local)}
			u, _ := newTestUpgrader(t, f, "1.2.3", nil)
			rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{
				{Name: "torn", Version: c.local, Upstream: "o/up", Replace: r},
			})
			if err != nil {
				t.Fatalf("UpgradePlugins: %v", err)
			}
			if got := rep.Results[0].Action; got != c.want {
				t.Errorf("结论 = %q，期望 %q", got, c.want)
			}
			if len(r.calls) != 0 || f.assetHits.Load() != 0 {
				t.Error("不比磁盘新时既不该下载也不该替换")
			}
		})
	}
}

// 上游没有 digest → 跳过（上游没有校验和），并且**一个字节都不下**。
// 不能退化成"没校验也装"：stagePlugin 的"没有 sha256 就不装"是唯一口径。
func TestUpgradePluginsSkipsWhenUpstreamHasNoDigest(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	up := f.addUpstream("o/up", "v9.9.9")
	up.addPlugin("torn", "0.2.0", fakePluginAsset(t, "torn", "0.2.0"), false)

	r := &fakeReplacer{bin: fakePluginBin(t, t.TempDir(), "torn", "0.1.0")}
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)
	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{
		{Name: "torn", Version: "0.1.0", Upstream: "o/up", Replace: r},
	})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	if got := rep.Results[0].Action; got != "跳过（上游没有校验和）" {
		t.Errorf("结论 = %q，期望「跳过（上游没有校验和）」", got)
	}
	if n := f.assetHits.Load(); n != 0 {
		t.Errorf("没有校验和时不该下载，实际命中 %d 次", n)
	}
	if len(r.calls) != 0 {
		t.Error("没有校验和时不该换文件")
	}
}

// 失败分类表第四行：产物自称版本与文件名不符 → **失败**（不能降级成跳过）。
// 那是投毒或发错产物的信号，装上去的症状是"升完版本号没变"。
func TestUpgradePluginsRejectsUpstreamVersionMismatch(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	up := f.addUpstream("o/up", "v9.9.9")
	// 文件名说 0.2.0，产物自称 0.1.9。
	up.addPlugin("torn", "0.2.0", fakePluginAsset(t, "torn", "0.1.9"), true)

	r := &fakeReplacer{bin: fakePluginBin(t, t.TempDir(), "torn", "0.1.0")}
	before := readFile(t, r.bin)
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{
		{Name: "torn", Version: "0.1.0", Upstream: "o/up", Replace: r},
	})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	got := rep.Results[0].Action
	if !strings.HasPrefix(got, "失败") {
		t.Fatalf("版本不符必须判失败，实际 %q", got)
	}
	for _, want := range []string{"0.1.9", "0.2.0", "文件名"} {
		if !strings.Contains(got, want) {
			t.Errorf("失败文案缺少 %q：%q", want, got)
		}
	}
	if len(r.calls) != 0 {
		t.Error("版本不符时不该换文件")
	}
	if readFile(t, r.bin) != before {
		t.Error("旧文件被改动了")
	}
	if _, err := os.Stat(PluginStagedPath(r.bin)); !os.IsNotExist(err) {
		t.Error("被拒绝的产物不该留在落位路径上")
	}
}

// 安全边界：Manifest.Upstream 只认 owner/repo。
//
// 含 URL、含 ":"、含多段路径的一律无效 → 跳过并 warn，且**一个请求都不发**
// （否则 manifest 就成了"指定任意下载源"的开关）。
func TestUpgradePluginsRejectsInvalidUpstream(t *testing.T) {
	bad := []string{
		"https://evil.example/o/up",
		"git@github.com:o/up.git",
		"o/up/extra",
		"/o/up",
		"o/up ",
		"o",
		"../up",
		"o/..",
	}
	for _, up := range bad {
		t.Run(up, func(t *testing.T) {
			f := newFakeRelease(t, "1.2.4", peHeader("core"))
			f.addUpstream("o/up", "v9.9.9")

			r := &fakeReplacer{bin: fakePluginBin(t, t.TempDir(), "torn", "0.1.0")}
			u, _ := newTestUpgrader(t, f, "1.2.3", nil)
			rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{
				{Name: "torn", Version: "0.1.0", Upstream: up, Replace: r},
			})
			if err != nil {
				t.Fatalf("无效上游只该跳过，不该报错: %v", err)
			}
			if got := rep.Results[0].Action; got != "跳过（上游声明无效）" {
				t.Errorf("结论 = %q，期望「跳过（上游声明无效）」", got)
			}
			if n := f.apiHits.Load() + f.upstreamHits.Load() + f.assetHits.Load(); n != 0 {
				t.Errorf("无效上游不该发任何请求，实际 %d 次", n)
			}
			if len(r.calls) != 0 {
				t.Error("无效上游不该换文件")
			}
		})
	}
}

// 混合来源：索引里的按索引序在前，其余按插件名升序在后；每条各自写来源。
//
// 上游 tag 与核心 tag 同名（都叫 v1.2.4），两个仓的资产列表不同——来源写错
// 或查错仓都会在这里露出来。
func TestUpgradePluginsMixedSourcesOrderAndReport(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	f.addPlugin("alpha", "0.2.0", fakePluginAsset(t, "alpha", "0.2.0"))
	f.addPlugin("zeta", "0.2.0", fakePluginAsset(t, "zeta", "0.2.0"))
	up := f.addUpstream("o/up", "v1.2.4")
	up.addPlugin("torn", "0.2.0", fakePluginAsset(t, "torn", "0.2.0"), true)
	up.addPlugin("beacon", "0.2.0", fakePluginAsset(t, "beacon", "0.2.0"), true)

	_, alpha := pluginTarget(t, "alpha")
	_, zeta := pluginTarget(t, "zeta")
	_, torn := pluginTarget(t, "torn")
	_, beacon := pluginTarget(t, "beacon")
	torn.Upstream = "o/up"
	beacon.Upstream = "o/up"

	// 故意打乱入参：报告顺序必须由索引 + 插件名决定，两次执行可逐行比对。
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)
	rep, err := u.UpgradePlugins(context.Background(),
		[]PluginTarget{torn, zeta, beacon, alpha})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	var names []string
	for _, d := range rep.Results {
		names = append(names, d.Name)
	}
	want := []string{"alpha", "zeta", "beacon", "torn"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("顺序 = %v，期望 %v（索引序在前、其余按名升序）", names, want)
	}
	if got := rep.Tag; got != "v1.2.4" {
		t.Errorf("报告的核心 tag = %q，期望 v1.2.4", got)
	}
	for _, d := range rep.Results {
		want := "o/up@v1.2.4"
		if d.Name == "alpha" || d.Name == "zeta" {
			want = "o/r@v1.2.4"
		}
		if d.Source != want {
			t.Errorf("%s 的来源 = %q，期望 %q", d.Name, d.Source, want)
		}
		if d.Action != "升" {
			t.Errorf("%s 应升，实际 %q", d.Name, d.Action)
		}
	}
	text := rep.Text()
	for _, want := range []string{"（来源 o/r@v1.2.4）", "（来源 o/up@v1.2.4）"} {
		if !strings.Contains(text, want) {
			t.Errorf("报告缺少 %q:\n%s", want, text)
		}
	}
}

// 有 token：上游产物走 api_base 的资产端点，镜像通道一次都不碰
// （私有仓的产物在镜像上取不到，token 更不能发给镜像）。
func TestUpgradePluginsUpstreamWithTokenUsesAPIAssetEndpoint(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("core"))
	up := f.addUpstream("o/up", "v9.9.9")
	blob := fakePluginAsset(t, "torn", "0.2.0")
	up.addPlugin("torn", "0.2.0", blob, true)

	r := &fakeReplacer{bin: fakePluginBin(t, t.TempDir(), "torn", "0.1.0")}
	u, _ := newTestUpgrader(t, f, "1.2.3", func(o *Options) { o.Token = "secret" })

	rep, err := u.UpgradePlugins(context.Background(), []PluginTarget{
		{Name: "torn", Version: "0.1.0", Upstream: "o/up", Replace: r},
	})
	if err != nil {
		t.Fatalf("UpgradePlugins: %v", err)
	}
	if got := rep.Results[0].Action; got != "升" {
		t.Fatalf("有 token 时也应能升，实际 %q（%s）", got, rep.Text())
	}
	if !bytes.Equal(r.got, blob) {
		t.Error("换上去的字节与上游产物不一致")
	}
	if n := f.assetHits.Load(); n != 0 {
		t.Errorf("有 token 时不该碰镜像通道，实际命中 %d 次", n)
	}
	if f.authOnAsset.Load() {
		t.Error("token 被发给了 asset_base 的主机")
	}
}

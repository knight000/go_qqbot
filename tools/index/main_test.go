package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture 是一份最小的「已跑过 tools/build plugins」的产物树：
// dist/<os>_<arch>/<插件名>[.exe]，内容按平台与插件名区分，
// 这样索引里任何一处张冠李戴都会让 sha256 对不上。
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	plugins, err := bundledPlugins()
	if err != nil {
		t.Fatalf("读取插件清单: %v", err)
	}
	for _, tgt := range releasePlatforms {
		dir := filepath.Join(dist, tgt.dir())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, p := range plugins {
			content := artifactContent(p.name, tgt)
			if err := os.WriteFile(filepath.Join(dir, p.name+exeSuffix(tgt.os)), content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func artifactContent(plugin string, tgt platform) []byte {
	return []byte("artifact:" + plugin + "@" + tgt.String())
}

func readIndex(t *testing.T, path string) index {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 index.json: %v", err)
	}
	var idx index
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("解析 index.json: %v", err)
	}
	return idx
}

// index.json 是 /upgrade plugin 的唯一事实源：schema 版本、核心版本、
// 四个插件 × 三个平台，一个都不能少。
func TestGenerateListsEveryPluginOnEveryPlatform(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	staging := filepath.Join(root, "dist", "plugins")

	err := generate(options{
		tag:     "v0.0.0-p3-smoke",
		repo:    "knight000/go_qqbot",
		dist:    dist,
		staging: staging,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	idx := readIndex(t, filepath.Join(staging, "index.json"))
	if idx.Schema != schemaVersion {
		t.Errorf("schema = %d, want %d", idx.Schema, schemaVersion)
	}
	// 版本号只经 -X buildinfo.Version 注入，值是去掉前导 v 的 tag。
	if idx.Core.Version != "0.0.0-p3-smoke" {
		t.Errorf("core.version = %q, want 0.0.0-p3-smoke", idx.Core.Version)
	}

	// 插件名是集合而不是顺序：只比较内容，顺序另有断言（见下）。
	got := make(map[string]pluginEntry, len(idx.Plugins))
	for _, p := range idx.Plugins {
		got[p.Name] = p
	}
	if len(idx.Plugins) != 4 {
		t.Fatalf("plugins 数量 = %d, want 4: %+v", len(idx.Plugins), idx.Plugins)
	}
	for _, name := range []string{"adminping", "bootnotice", "echo", "md"} {
		p, ok := got[name]
		if !ok {
			t.Errorf("缺少插件 %s", name)
			continue
		}
		if p.Version == "" {
			t.Errorf("%s: version 为空（应来自 Manifest()）", name)
		}
		if len(p.Assets) != len(releasePlatforms) {
			t.Errorf("%s: assets 数量 = %d, want %d", name, len(p.Assets), len(releasePlatforms))
		}
		for _, tgt := range releasePlatforms {
			a, ok := p.Assets[tgt.String()]
			if !ok {
				t.Errorf("%s: 缺少 %s 资产", name, tgt)
				continue
			}
			if a.SHA256 == "" {
				t.Errorf("%s/%s: sha256 为空", name, tgt)
			}
			if !strings.HasPrefix(a.URL, "https://github.com/knight000/go_qqbot/releases/download/v0.0.0-p3-smoke/") {
				t.Errorf("%s/%s: url = %q", name, tgt, a.URL)
			}
		}
	}

	// 名字按字典序：两次生成的 diff 才有意义。
	for i := 1; i < len(idx.Plugins); i++ {
		if idx.Plugins[i-1].Name >= idx.Plugins[i].Name {
			t.Errorf("插件未按字典序排列: %s 在 %s 之前", idx.Plugins[i-1].Name, idx.Plugins[i].Name)
		}
	}
}

// 资产的 URL 末段必须与上传到 release 的文件名逐字一致，否则下载 404；
// 而文件名是 index 自己算出来的，所以这条断言同时锁住了两边。
func TestAssetURLMatchesStagedFileName(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	staging := filepath.Join(root, "dist", "plugins")

	if err := generate(options{tag: "v1.2.3", repo: "o/r", dist: dist, staging: staging}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	idx := readIndex(t, filepath.Join(staging, "index.json"))
	for _, p := range idx.Plugins {
		for key, a := range p.Assets {
			base := a.URL[strings.LastIndexByte(a.URL, '/')+1:]
			if _, err := os.Stat(filepath.Join(staging, base)); err != nil {
				t.Errorf("%s/%s: URL 指向 %s，但 staging 里没有同名文件: %v", p.Name, key, base, err)
			}
		}
	}
}

// 每个 (插件, 平台) 的 sha256 必须各不相同：全都相同说明索引把同一份产物
// 抄了 N 遍，或者根本没读文件。
func TestEveryAssetHasItsOwnSHA256(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	staging := filepath.Join(root, "dist", "plugins")
	if err := generate(options{tag: "v1.0.0", repo: "o/r", dist: dist, staging: staging}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	seen := map[string]string{}
	idx := readIndex(t, filepath.Join(staging, "index.json"))
	for _, p := range idx.Plugins {
		for key, a := range p.Assets {
			who := p.Name + "/" + key
			if prev, dup := seen[a.SHA256]; dup {
				t.Errorf("%s 与 %s 的 sha256 相同（%s），索引没读到各自的产物", who, prev, a.SHA256)
			}
			seen[a.SHA256] = who
		}
	}
}

// sha256 的算法与格式是外部契约（sha256sum -c 与 /upgrade 的校验要用它），
// 所以钉一个已知答案：sha256("abc")。
func TestSHA256IsHexOfTheActualBytes(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	staging := filepath.Join(root, "dist", "plugins")

	// 只把 echo 的 linux/amd64 产物换成 "abc"，其余保持夹具内容。
	echo := filepath.Join(dist, "linux_amd64", "echo")
	if err := os.WriteFile(echo, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generate(options{tag: "v1.0.0", repo: "o/r", dist: dist, staging: staging}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	idx := readIndex(t, filepath.Join(staging, "index.json"))
	var got string
	for _, p := range idx.Plugins {
		if p.Name == "echo" {
			got = p.Assets["linux-amd64"].SHA256
		}
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Errorf("echo/linux-amd64 sha256 = %q, want %q", got, want)
	}
}

// 少一个平台的产物就必须报错停下：发出去的 index.json 指向 404 的资产，
// 比不发更糟——所有者只会看到 /upgrade plugin 静默失败。
func TestGenerateFailsWhenArtifactIsMissing(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	staging := filepath.Join(root, "dist", "plugins")

	if err := os.Remove(filepath.Join(dist, "linux_arm64", "md")); err != nil {
		t.Fatal(err)
	}
	err := generate(options{tag: "v1.0.0", repo: "o/r", dist: dist, staging: staging})
	if err == nil {
		t.Fatal("缺少产物时应当报错")
	}
	// 错误里要能直接看出「缺谁、哪个平台、怎么补」，否则 CI 日志只剩一句 ENOENT。
	for _, want := range []string{"md", "linux_arm64", "tools/build plugins"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(staging, "index.json")); statErr == nil {
		t.Error("失败时不该留下半份 index.json")
	}
}

// 发布资产名是下载 URL 的末段，属于对外契约（ADR-0006 决策 11 的插件补充）：
// 扁平命名，带插件名与版本，windows 带 .exe。
func TestAssetName(t *testing.T) {
	cases := []struct {
		plugin, version string
		tgt             platform
		want            string
	}{
		{"echo", "0.1.0", platform{"linux", "amd64"}, "qqbot-plugin-echo-0.1.0-linux-amd64"},
		{"echo", "0.1.0", platform{"linux", "arm64"}, "qqbot-plugin-echo-0.1.0-linux-arm64"},
		{"echo", "0.1.0", platform{"windows", "amd64"}, "qqbot-plugin-echo-0.1.0-windows-amd64.exe"},
	}
	for _, c := range cases {
		if got := assetName(c.plugin, c.version, c.tgt); got != c.want {
			t.Errorf("assetName(%s, %s, %s) = %q, want %q", c.plugin, c.version, c.tgt, got, c.want)
		}
	}
}

// 插件版本来自 Manifest()，不是手写常量：版本升级只改插件包，索引自动跟上。
func TestPluginVersionComesFromManifest(t *testing.T) {
	plugins, err := bundledPlugins()
	if err != nil {
		t.Fatalf("bundledPlugins: %v", err)
	}
	if len(plugins) == 0 {
		t.Fatal("没有插件")
	}
	for _, p := range plugins {
		if p.version == "" || p.version == "dev" {
			t.Errorf("%s 的 Manifest().Version = %q，发布索引需要真实版本号", p.name, p.version)
		}
	}
}

// staging 里的文件必须与产物逐字节相同：上传的是这份拷贝，不是原路径。
func TestStagedCopyMatchesArtifact(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	staging := filepath.Join(root, "dist", "plugins")
	if err := generate(options{tag: "v1.0.0", repo: "o/r", dist: dist, staging: staging}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	src := filepath.Join(dist, "windows_amd64", "bootnotice.exe")
	dst := filepath.Join(staging, assetName("bootnotice", pluginVersion(t, "bootnotice"), platform{"windows", "amd64"}))
	a, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读取 staging 拷贝: %v", err)
	}
	if string(a) != string(b) {
		t.Error("staging 拷贝与产物内容不一致")
	}
	// 顺便确认索引里记的就是这份内容的 sha256。
	idx := readIndex(t, filepath.Join(staging, "index.json"))
	for _, p := range idx.Plugins {
		if p.Name != "bootnotice" {
			continue
		}
		sum := sha256.Sum256(b)
		if got, want := p.Assets["windows-amd64"].SHA256, hex.EncodeToString(sum[:]); got != want {
			t.Errorf("index sha256 = %s, want %s", got, want)
		}
	}
}

func pluginVersion(t *testing.T, name string) string {
	t.Helper()
	plugins, err := bundledPlugins()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plugins {
		if p.name == name {
			return p.version
		}
	}
	t.Fatalf("没有插件 %s", name)
	return ""
}

// -tag 是必需的：没有它拼不出下载 URL，也推不出 core.version。
func TestParseArgsRequiresTag(t *testing.T) {
	if _, err := parseArgs([]string{"-repo", "o/r"}); err == nil {
		t.Fatal("缺 -tag 时应当报错")
	}
	o, err := parseArgs([]string{"-tag", "v1.2.3", "-dist", "d", "-staging", "s"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if o.tag != "v1.2.3" || o.dist != "d" || o.staging != "s" {
		t.Errorf("解析结果 = %+v", o)
	}
	// 默认值：repo 指向本仓，out 落在 staging 目录里。
	if o.repo != defaultRepo {
		t.Errorf("repo 默认值 = %q, want %q", o.repo, defaultRepo)
	}
	if o.out != "" {
		t.Errorf("out 默认值 = %q，应当空着由 staging 推导", o.out)
	}
}

// makePrevious 造一份"上一次发布"的 index.json：先用 generate 生成，
// 再把指定插件的版本改低（模拟"它上次发的是旧版本"）。其余插件与当前
// 版本相同，应当被沿用。
func makePrevious(t *testing.T, root string, oldVersions map[string]string) string {
	t.Helper()
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	dir := filepath.Join(root, "old")
	out := filepath.Join(dir, "index.json")
	if err := generate(options{tag: "v1.0.0", repo: "o/r", dist: dist, staging: dir, out: out}); err != nil {
		t.Fatalf("生成上一版 index: %v", err)
	}
	if len(oldVersions) == 0 {
		return out
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var idx index
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	for i := range idx.Plugins {
		if v, ok := oldVersions[idx.Plugins[i].Name]; ok {
			idx.Plugins[i].Version = v
			for key, a := range idx.Plugins[i].Assets {
				// 资产名里带版本，改版本时一并改掉，保持"上一版长什么样"自洽。
				parts := strings.SplitN(key, "-", 2)
				a.URL = "https://github.com/o/r/releases/download/v1.0.0/" +
					assetName(idx.Plugins[i].Name, v, platform{os: parts[0], arch: parts[1]})
				idx.Plugins[i].Assets[key] = a
			}
		}
	}
	data, err = json.MarshalIndent(idx, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// 版本相同的插件原样沿用上一条目：URL/sha256 都指向上一版 tag，且**不拷进
// staging**——staging 里有什么就上传什么，不拷就不会被 gh release upload 带走。
func TestGenerateCarriesForwardUnchangedPlugins(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	prev := makePrevious(t, root, nil)
	staging := filepath.Join(root, "new")

	// 模拟上一次本地运行留下的残留：沿用的条目不该把它一起上传。
	stale := filepath.Join(staging, "qqbot-plugin-echo-stale-windows-amd64.exe")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := generate(options{
		tag: "v1.1.0", repo: "o/r", dist: dist, staging: staging, previous: prev,
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	idx := readIndex(t, filepath.Join(staging, "index.json"))
	if len(idx.Plugins) != 4 {
		t.Fatalf("沿用后仍应登记 4 个插件，实际 %d", len(idx.Plugins))
	}
	for _, p := range idx.Plugins {
		for key, a := range p.Assets {
			if !strings.Contains(a.URL, "/v1.0.0/") {
				t.Errorf("%s/%s 应沿用上一版 tag，实际 %q", p.Name, key, a.URL)
			}
			if a.SHA256 == "" {
				t.Errorf("%s/%s 沿用时 sha256 不能为空", p.Name, key)
			}
		}
	}

	// staging 里除了 index.json 不该有别的文件：本次没有任何插件更新。
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "index.json" {
			t.Errorf("沿用的插件不该出现在 staging 里，实际有 %s", e.Name())
		}
	}
}

// 只有涨了版本的插件才重新产出资产：其余 3 个沿用，1 个新发 × 3 平台。
func TestGenerateUploadsOnlyChangedPlugins(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	prev := makePrevious(t, root, map[string]string{"md": "0.0.1"})
	staging := filepath.Join(root, "new")

	if err := generate(options{
		tag: "v1.1.0", repo: "o/r", dist: dist, staging: staging, previous: prev,
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	idx := readIndex(t, filepath.Join(staging, "index.json"))
	mdVer := pluginVersion(t, "md")
	var newAssets int
	for _, p := range idx.Plugins {
		for key, a := range p.Assets {
			switch p.Name {
			case "md":
				if p.Version != mdVer {
					t.Errorf("md 的版本应更新到 %s，实际 %s", mdVer, p.Version)
				}
				if !strings.Contains(a.URL, "/v1.1.0/") {
					t.Errorf("md/%s 应指向本次 tag，实际 %q", key, a.URL)
				}
				newAssets++
			default:
				if !strings.Contains(a.URL, "/v1.0.0/") {
					t.Errorf("%s/%s 应沿用上一版 tag，实际 %q", p.Name, key, a.URL)
				}
			}
		}
	}
	if newAssets != len(releasePlatforms) {
		t.Errorf("本次新发资产数 = %d，期望 %d", newAssets, len(releasePlatforms))
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if e.Name() == "index.json" {
			continue
		}
		files = append(files, e.Name())
		if !strings.HasPrefix(e.Name(), "qqbot-plugin-md-") {
			t.Errorf("staging 里出现了未变化的插件资产: %s", e.Name())
		}
	}
	if len(files) != len(releasePlatforms) {
		t.Errorf("staging 里的插件资产数 = %d，期望 %d: %v", len(files), len(releasePlatforms), files)
	}
}

// 没有上一条目（新增插件）或没有 -previous → 照旧全量产出。
func TestGenerateWithoutPreviousUploadsEverything(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	staging := filepath.Join(root, "new")

	if err := generate(options{tag: "v1.1.0", repo: "o/r", dist: dist, staging: staging}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "qqbot-plugin-") {
			n++
		}
	}
	if want := len(releasePlatforms) * 4; n != want {
		t.Errorf("没有 -previous 时应全量产出 %d 个资产，实际 %d", want, n)
	}
}

// 上一版 index.json 读不动时直接报错：拿半份清单去"沿用"会让某些插件
// 永远发不出去，比流水线红掉更难查。
func TestGenerateRejectsBrokenPrevious(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	bad := filepath.Join(root, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := generate(options{
		tag: "v1.1.0", repo: "o/r", dist: dist, staging: filepath.Join(root, "new"), previous: bad,
	})
	if err == nil {
		t.Fatal("上一版 index.json 解析失败时应当报错")
	}
	if !strings.Contains(err.Error(), "上一版") {
		t.Errorf("错误串应说明是上一版清单的问题，实际: %v", err)
	}
}

// 上一版里某个插件的资产不全，但它本次涨了版本 → 不影响发布（反正要重发）。
// 只有"要沿用的条目"才必须完整，否则上一版的一次残缺会永久挡住后续发布。
func TestGenerateIgnoresBrokenEntryWhenVersionChanged(t *testing.T) {
	root := fixture(t)
	dist := filepath.Join(root, "internal", "pluginbundle", "dist")
	prev := makePrevious(t, root, map[string]string{"md": "0.0.1"})

	data, err := os.ReadFile(prev)
	if err != nil {
		t.Fatal(err)
	}
	var idx index
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	for i := range idx.Plugins {
		if idx.Plugins[i].Name == "md" {
			delete(idx.Plugins[i].Assets, "linux-arm64")
		}
	}
	data, err = json.MarshalIndent(idx, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prev, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	staging := filepath.Join(root, "new")
	if err := generate(options{tag: "v1.1.0", repo: "o/r", dist: dist, staging: staging, previous: prev}); err != nil {
		t.Fatalf("涨了版本的插件不该被上一版的残缺挡住: %v", err)
	}
	var mdAssets int
	for _, e := range mustReadDir(t, staging) {
		if strings.HasPrefix(e.Name(), "qqbot-plugin-md-") {
			mdAssets++
		}
	}
	if mdAssets != len(releasePlatforms) {
		t.Errorf("md 应重发 %d 个平台，实际 %d", len(releasePlatforms), mdAssets)
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeRelease 是一份「发布」的替身：两个 httptest 服务器分别扮演
// api_base（元数据通道）与 asset_base（镜像通道）。
//
// 它的存在就是为了把两条通道分开：任何「元数据从镜像取」或「token 发给
// 镜像」的实现错误都能被命中计数抓住，而不用读代码确认。
type fakeRelease struct {
	t        *testing.T
	version  string
	tag      string
	binary   []byte
	index    string
	sums     string
	coreName string
	// indexAssetName 是清单在 release 资产列表里的名字。默认 "index.json"
	// （GitHub 不允许资产名含 "/"）；测试可以把它改成别的形状来验证匹配口径。
	indexAssetName string

	// pluginIdx 是索引里插件条目的 JSON 片段（保持插入顺序，报告的排序要可断言）。
	pluginIdx []string
	// pluginBlobs 是镜像通道按**资产名**提供的插件产物。
	pluginBlobs map[string][]byte
	// latestNames 是「最新发布」资产列表里的插件产物名。addPluginAtTag 登记到
	// 旧 tag 的产物故意不在这里：沿用条目必须按 URL 里的 tag 去查，而不是
	// 拿最新发布的资产列表碰运气。
	latestNames []string
	// blobTags 记录每个产物登记在哪个 tag 上：镜像通道只在该 tag 的路径下
	// 提供它，拿当前 tag 去拼地址会 404（正是 #52 要防的那个错）。
	blobTags map[string]string

	api   *httptest.Server
	asset *httptest.Server

	// upstreams 是上游仓库的替身（#53）：与核心仓共用同一对 httptest 服务器
	// （api_base 同主机是硬约定），但路径与资产列表完全独立。
	upstreams []*fakeUpstream

	apiHits   atomic.Int64
	tagHits   atomic.Int64
	assetHits atomic.Int64
	// upstreamHits 只数上游仓的元数据请求：核心仓的索引与它无关。
	upstreamHits atomic.Int64
	// authOnAsset 记录镜像通道上出现过的 Authorization 头（必须永远为空）。
	authOnAsset atomic.Bool
	// broken 为 true 时所有请求 500，用来断言"一个请求都不发"。
	broken atomic.Bool
}

// fakeUpstream 是一个上游仓库的替身：它只有插件产物，没有 index.json，
// 也不属于核心仓。
//
// tag 故意允许与核心仓同名——同一个 tag 名在两个仓含义不同，这正是
// "上游必须按自己的仓构造 client"要防的那个错。
type fakeUpstream struct {
	repo   string
	tag    string
	assets []fakeUpstreamAsset
	// status 非 0 时所有请求都回它（404 发布不存在 / 500 上游坏了）。
	status int
	// brokenJSON 让 releases/latest 回一段不是 JSON 的内容。
	brokenJSON bool
}

type fakeUpstreamAsset struct {
	name string
	blob []byte
	// digest 是 GitHub API 的 digest 字段（"sha256:<hex>"）。空串模拟
	// "上游没给校验和"。
	digest string
}

func newFakeRelease(t *testing.T, version string, binary []byte) *fakeRelease {
	t.Helper()
	f := &fakeRelease{
		t:              t,
		version:        version,
		tag:            "v" + version,
		binary:         binary,
		coreName:       CoreAssetName(version, runtime.GOOS, runtime.GOARCH),
		indexAssetName: "index.json",
		pluginBlobs:    map[string][]byte{},
		blobTags:       map[string]string{},
	}
	sum := sha256.Sum256(binary)
	// index.json 与 tools/index 的输出同形。默认那条 echo 只是个占位条目
	// （地址与哈希都是假的）——#44 的预览用例只看版本比较，不看产物。
	// 真的要下载插件的用例用 addPlugin 换成真产物。
	f.index = fmt.Sprintf(`{
  "schema": 1,
  "core": {"version": %q},
  "plugins": [
    {"name": "echo", "version": "0.2.0",
     "assets": {"%s": {"url": "https://example.invalid/echo", "sha256": "%s"}}}
  ]
}`, version, currentPlatform(), strings.Repeat("ab", 32))
	f.sums = fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), f.coreName)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.apiHits.Add(1)
		if f.broken.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": f.tag,
			"assets":   f.assetList(f.latestNames),
		})
	})
	// 按 tag 取发布：沿用条目指向旧 tag 时，有 token 的客户端走这条路。
	mux.HandleFunc("/repos/o/r/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		f.apiHits.Add(1)
		f.tagHits.Add(1)
		if f.broken.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": path.Base(r.URL.Path),
			"assets":   f.assetList(f.pluginAssetNames()),
		})
	})
	mux.HandleFunc("/repos/o/r/releases/assets/", func(w http.ResponseWriter, r *http.Request) {
		f.apiHits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/1"):
			_, _ = w.Write([]byte(f.index))
		case strings.HasSuffix(r.URL.Path, "/2"):
			_, _ = w.Write([]byte(f.sums))
		case strings.HasSuffix(r.URL.Path, "/3"):
			_, _ = w.Write(f.binary)
		default:
			// 插件产物：id 从 101 起按资产名顺序分配，这里按名字反查。
			if blob, ok := f.pluginByID(r.URL.Path); ok {
				_, _ = w.Write(blob)
				return
			}
			http.NotFound(w, r)
		}
	})
	// 上游仓的元数据：/repos/<owner>/<repo>/releases/{latest|assets/<id>}。
	// 注册成子树，让每个用例自己决定扮演哪个仓；核心仓的路径更长，优先级更高。
	mux.HandleFunc("/repos/", f.serveUpstreamAPI)
	f.api = httptest.NewServer(mux)
	t.Cleanup(f.api.Close)

	amux := http.NewServeMux()
	amux.HandleFunc("/o/r/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		f.assetHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			f.authOnAsset.Store(true)
		}
		if f.broken.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if blob, ok := f.pluginBlobs[path.Base(r.URL.Path)]; ok {
			// 只在该产物登记的 tag 路径下提供：拿当前 tag 去拼旧资产会 404。
			if tag := f.blobTags[path.Base(r.URL.Path)]; tag != "" &&
				!strings.Contains(r.URL.Path, "/download/"+tag+"/") {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(blob)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/"+f.coreName) {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(f.binary)
	})
	// 上游仓的产物直链：/owner/repo/releases/download/<tag>/<asset>。
	amux.HandleFunc("/", f.serveUpstreamAsset)
	f.asset = httptest.NewServer(amux)
	t.Cleanup(f.asset.Close)
	return f
}

// addUpstream 让假 API 扮演一个上游仓库（另一个 owner/repo）。
func (f *fakeRelease) addUpstream(repo, tag string) *fakeUpstream {
	f.t.Helper()
	u := &fakeUpstream{repo: repo, tag: tag}
	f.upstreams = append(f.upstreams, u)
	return u
}

func (f *fakeRelease) upstream(repo string) *fakeUpstream {
	for _, u := range f.upstreams {
		if u.repo == repo {
			return u
		}
	}
	return nil
}

// addPlugin 往上游的最新发布里加一份本平台产物，名字与 tools/index 同形。
//
// withDigest=false 模拟 GitHub 返回的 digest 为空：没有校验和就没有可校验的
// 哈希，只能跳过（与 stagePlugin 的"没有 sha256 就不装"同一口径）。
func (u *fakeUpstream) addPlugin(plugin, version string, blob []byte, withDigest bool) string {
	name := "qqbot-plugin-" + plugin + "-" + version + "-" + currentPlatform() + exeSuffix()
	u.addRaw(name, blob, withDigest)
	return name
}

// addRaw 按**给定的资产名**登记一份产物：造"没有本平台产物"的现场时用得上
// （名字里的平台段不是本机的）。
func (u *fakeUpstream) addRaw(name string, blob []byte, withDigest bool) {
	a := fakeUpstreamAsset{name: name, blob: blob}
	if withDigest {
		a.digest = "sha256:" + sha256Hex(blob)
	}
	u.assets = append(u.assets, a)
}

// serveUpstreamAPI 扮演上游仓的元数据通道。路径形状：
// /repos/<owner>/<repo>/releases/latest 与 /repos/<owner>/<repo>/releases/assets/<id>。
func (f *fakeRelease) serveUpstreamAPI(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) < 5 || segs[0] != "repos" {
		http.NotFound(w, r)
		return
	}
	up := f.upstream(segs[1] + "/" + segs[2])
	if up == nil {
		http.NotFound(w, r)
		return
	}
	f.upstreamHits.Add(1)
	if up.status != 0 {
		http.Error(w, "boom", up.status)
		return
	}
	switch {
	case len(segs) == 5 && segs[3] == "releases" && segs[4] == "latest":
		if up.brokenJSON {
			_, _ = w.Write([]byte("<html>not json</html>"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": up.tag,
			"assets":   up.assetList(),
		})
	case len(segs) == 6 && segs[3] == "releases" && segs[4] == "assets":
		var id int64
		if _, err := fmt.Sscanf(segs[5], "%d", &id); err != nil {
			http.NotFound(w, r)
			return
		}
		if i := int(id - upstreamAssetIDBase); i >= 0 && i < len(up.assets) {
			_, _ = w.Write(up.assets[i].blob)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// upstreamAssetIDBase 是上游产物的资产 id 起点：核心仓用 1/2/3 与 101 起，
// 这里错开，误用核心仓的 id 会取不到字节。
const upstreamAssetIDBase = 201

func (u *fakeUpstream) assetList() []map[string]any {
	out := make([]map[string]any, 0, len(u.assets))
	for i, a := range u.assets {
		item := map[string]any{
			"id": int64(upstreamAssetIDBase + i), "name": a.name, "size": len(a.blob),
		}
		// digest 为空时 GitHub 省略该字段，这里同样省略。
		if a.digest != "" {
			item["digest"] = a.digest
		}
		out = append(out, item)
	}
	return out
}

// serveUpstreamAsset 扮演上游仓的产物直链（无 token 时走这条）。
// 只在该产物登记的 tag 下提供：拿别的 tag 去拼地址会 404。
func (f *fakeRelease) serveUpstreamAsset(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) < 6 || segs[2] != "releases" || segs[3] != "download" {
		http.NotFound(w, r)
		return
	}
	up := f.upstream(segs[0] + "/" + segs[1])
	if up == nil {
		http.NotFound(w, r)
		return
	}
	f.assetHits.Add(1)
	if r.Header.Get("Authorization") != "" {
		f.authOnAsset.Store(true)
	}
	if f.broken.Load() {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	tag, name := segs[4], segs[5]
	if tag != up.tag {
		http.NotFound(w, r)
		return
	}
	for _, a := range up.assets {
		if a.name == name {
			_, _ = w.Write(a.blob)
			return
		}
	}
	http.NotFound(w, r)
}

// addPlugin 让这次"发布"带上一个插件产物：索引里写本平台资产（地址是
// tools/index 生成的那种 github 直链），镜像通道按资产名提供字节。
//
// 返回资产名。索引会按插入顺序重建，所以报告的排序可以直接断言。
func (f *fakeRelease) addPlugin(name, version string, blob []byte) string {
	f.t.Helper()
	return f.addPluginAtTag(name, version, f.tag, blob)
}

// addPluginAtTag 把插件产物登记到**指定 tag** 的地址上：沿用条目（#52）的
// 现场——本次发布的 index.json 里，这个插件的 url 指向上一版 tag。
func (f *fakeRelease) addPluginAtTag(name, version, tag string, blob []byte) string {
	f.t.Helper()
	asset := "qqbot-plugin-" + name + "-" + version + "-" + currentPlatform() + exeSuffix()
	sha := sha256Hex(blob)
	f.pluginIdx = append(f.pluginIdx, fmt.Sprintf(
		`{"name": %q, "version": %q, "assets": {%q: {"url": %q, "sha256": %q}}}`,
		name, version, currentPlatform(),
		"https://github.com/o/r/releases/download/"+tag+"/"+asset, sha))
	f.pluginBlobs[asset] = blob
	f.blobTags[asset] = tag
	if tag == f.tag {
		f.latestNames = append(f.latestNames, asset)
	}
	f.rebuildIndex()
	return asset
}

// assetList 拼一份发布资产列表。pluginNames 是要列出的插件产物名；
// id 永远按全部插件产物的字典序分配，与 pluginByID 的反查口径一致。
func (f *fakeRelease) assetList(pluginNames []string) []map[string]any {
	assets := []map[string]any{
		{"id": 1, "name": f.indexAssetName, "size": len(f.index)},
		{"id": 2, "name": "checksums.txt", "size": len(f.sums)},
		{"id": 3, "name": f.coreName, "size": len(f.binary)},
	}
	idOf := map[string]int64{}
	for i, name := range f.pluginAssetNames() {
		idOf[name] = int64(101 + i)
	}
	for _, name := range pluginNames {
		assets = append(assets, map[string]any{
			"id": idOf[name], "name": name, "size": len(f.pluginBlobs[name]),
		})
	}
	return assets
}

// rewritePlugin 覆盖某个已加插件的索引版本（造"索引与产物自称不一致"的现场），
// 产物字节不动。
func (f *fakeRelease) rewritePluginVersion(name, version string) {
	f.t.Helper()
	for i, frag := range f.pluginIdx {
		if strings.Contains(frag, fmt.Sprintf("%q", name)) {
			var entry struct {
				Name    string `json:"name"`
				Version string `json:"version"`
				Assets  map[string]struct {
					URL    string `json:"url"`
					SHA256 string `json:"sha256"`
				} `json:"assets"`
			}
			if err := json.Unmarshal([]byte(frag), &entry); err != nil {
				f.t.Fatalf("解析索引片段: %v", err)
			}
			for plat, a := range entry.Assets {
				f.pluginIdx[i] = fmt.Sprintf(
					`{"name": %q, "version": %q, "assets": {%q: {"url": %q, "sha256": %q}}}`,
					entry.Name, version, plat, a.URL, a.SHA256)
			}
		}
	}
	f.rebuildIndex()
}

func (f *fakeRelease) rebuildIndex() {
	f.index = fmt.Sprintf(`{"schema":1,"core":{"version":%q},"plugins":[%s]}`,
		f.version, strings.Join(f.pluginIdx, ","))
}

// pluginAssetNames 按资产名排序返回已加入的插件产物名。
func (f *fakeRelease) pluginAssetNames() []string {
	names := make([]string, 0, len(f.pluginBlobs))
	for name := range f.pluginBlobs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// pluginByID 按 /releases/assets/<id> 反查插件产物：id 从 101 起、按资产名
// 排序分配，必须与 latest 里给出的资产列表对得上（否则 token 通道会取错产物）。
func (f *fakeRelease) pluginByID(rawPath string) ([]byte, bool) {
	i := strings.LastIndex(rawPath, "/")
	if i < 0 {
		return nil, false
	}
	var id int64
	if _, err := fmt.Sscanf(rawPath[i+1:], "%d", &id); err != nil {
		return nil, false
	}
	names := f.pluginAssetNames()
	idx := int(id - 101)
	if idx < 0 || idx >= len(names) {
		return nil, false
	}
	return f.pluginBlobs[names[idx]], true
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

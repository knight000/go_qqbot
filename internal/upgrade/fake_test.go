package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
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

	api   *httptest.Server
	asset *httptest.Server

	apiHits   atomic.Int64
	assetHits atomic.Int64
	// authOnAsset 记录镜像通道上出现过的 Authorization 头（必须永远为空）。
	authOnAsset atomic.Bool
	// broken 为 true 时所有请求 500，用来断言"一个请求都不发"。
	broken atomic.Bool
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
	}
	sum := sha256.Sum256(binary)
	// index.json 与 tools/index 的输出同形：插件资产指向 asset_base 风格
	// 的直链，但本票不消费它（插件升级是 #45）。
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
			"assets": []map[string]any{
				{"id": 1, "name": f.indexAssetName, "size": len(f.index)},
				{"id": 2, "name": "checksums.txt", "size": len(f.sums)},
				{"id": 3, "name": f.coreName, "size": len(f.binary)},
			},
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
			http.NotFound(w, r)
		}
	})
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
		if !strings.HasSuffix(r.URL.Path, "/"+f.coreName) {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(f.binary)
	})
	f.asset = httptest.NewServer(amux)
	t.Cleanup(f.asset.Close)
	return f
}

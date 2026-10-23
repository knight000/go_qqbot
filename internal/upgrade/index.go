package upgrade

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
)

// index.json 与 checksums.txt 是发布产物的两份清单（ADR-0006 决策 11）：
//
//	index.json     —— 插件版本与各平台资产的 URL/sha256，插件版本的事实源；
//	checksums.txt  —— sha256sum 的输出，覆盖核心产物。
//
// 两份都由 tools/index 与 CI 生成，本包只读。schema 字段只增不改：
// 客户端遇到不认识的 schema 必须拒绝，而不是把新字段当零值静默跳过。
const SchemaVersion = 1

// Index 是 plugins/index.json 的根对象，字段与 tools/index 逐字对应。
type Index struct {
	Schema  int           `json:"schema"`
	Core    IndexCore     `json:"core"`
	Plugins []IndexPlugin `json:"plugins"`
}

// IndexCore 只有版本号：核心产物的 sha256 在 checksums.txt 里，
// 而 index.json 由插件产物生成（决策 11 的构建顺序决定了它拿不到核心的哈希）。
type IndexCore struct {
	Version string `json:"version"`
}

type IndexPlugin struct {
	Name    string           `json:"name"`
	Version string           `json:"version"`
	Assets  map[string]Asset `json:"assets"`
}

// Asset 是一个平台产物的下载地址与校验和。键是 "<os>-<arch>"。
type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// ParseIndex 解析 index.json。
//
// schema 不认识就拒绝：把 v2 的清单按 v1 读，表现是"所有插件都没有资产"
// 或"版本号为空"，而那时升级已经开始了。
func ParseIndex(r io.Reader) (*Index, error) {
	var idx Index
	dec := json.NewDecoder(r)
	if err := dec.Decode(&idx); err != nil {
		return nil, fmt.Errorf("解析 index.json: %w", err)
	}
	if idx.Schema != SchemaVersion {
		return nil, fmt.Errorf("index.json 的 schema=%d，本版本只认 %d：请先升级到支持它的版本", idx.Schema, SchemaVersion)
	}
	if strings.TrimSpace(idx.Core.Version) == "" {
		return nil, fmt.Errorf("index.json 缺少 core.version")
	}
	return &idx, nil
}

// CoreAssetName 是核心产物在 release 资产列表里的名字。
//
// 与 tools/index 生成的插件资产名（qqbot-plugin-<name>-<ver>-<os>-<arch>）
// 同一套形状，靠 qqbot- 前缀区分。名称必须与 CI 的产物名逐字一致，
// 否则升级会在 404 上失败（而不是在校验上）。
func CoreAssetName(version, goos, goarch string) string {
	name := "qqbot-" + version + "-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// 发布清单在 release 资产里的文件名。GitHub 会把资产名拍平，
// 所以索引只能按文件名而不是路径匹配。
const (
	assetIndexName     = "index.json"
	assetChecksumsName = "checksums.txt"
)

// ParseChecksums 解析 sha256sum 的输出。
//
// 两种分隔符都要认：GNU coreutils 用 "sha  name"（两个空格），
// 二进制模式用 "sha *name"。CI 在 GNU 与 busybox 之间切换过一次就会
// 踩到这个差异，而症状是"校验和不匹配"——看起来像被投毒（决策 3/4）。
func ParseChecksums(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sum, name, ok := splitChecksumLine(line)
		if !ok {
			return nil, fmt.Errorf("checksums.txt 有无法解析的行: %q", line)
		}
		out[name] = strings.ToLower(sum)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取 checksums.txt: %w", err)
	}
	return out, nil
}

func splitChecksumLine(line string) (sum, name string, ok bool) {
	i := strings.IndexAny(line, " \t")
	if i <= 0 {
		return "", "", false
	}
	sum = line[:i]
	rest := strings.TrimLeft(line[i:], " \t")
	rest = strings.TrimPrefix(rest, "*") // 二进制模式
	if rest == "" || len(sum) != 64 {
		return "", "", false
	}
	return sum, rest, true
}

// currentPlatform 是本进程的资产键，例如 "windows-amd64"。
func currentPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// plugin 在 index 里按名字取一个插件条目（不看平台）。
func (i *Index) plugin(name string) (IndexPlugin, bool) {
	for _, p := range i.Plugins {
		if p.Name == name {
			return p, true
		}
	}
	return IndexPlugin{}, false
}

// lookup 在 index 里取某个插件的本平台资产。
func (i *Index) lookup(name string) (IndexPlugin, Asset, bool) {
	p, ok := i.plugin(name)
	if !ok {
		return IndexPlugin{}, Asset{}, false
	}
	a, ok := p.Assets[currentPlatform()]
	return p, a, ok
}

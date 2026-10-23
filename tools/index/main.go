// Command index 生成发布用的 plugins/index.json，并把插件产物整理成发布资产名。
//
//	go run ./tools/index -tag v1.2.3
//
// 它是发布流水线的最后一环（ADR-0006 决策 11）：核心与插件产物都编好之后，
// 把「哪个插件、哪个版本、哪个平台、下载地址、sha256」写成一份清单。
// /upgrade 只认这份清单——它同时是插件版本的事实源与完整性校验的来源。
//
// 版本号从各插件的 Manifest() 读（编译期导入，不执行产物），sha256 从磁盘上
// 真实产物算，下载 URL 按 GitHub Release 的资产地址拼：三者都不手写，
// 否则清单迟早与产物漂移，而漂移的表现是升级到一半才 404。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/plugins/adminping"
	"github.com/knight000/go_qqbot/plugins/bootnotice"
	"github.com/knight000/go_qqbot/plugins/echo"
	"github.com/knight000/go_qqbot/plugins/md"
)

const usageText = `用法：go run ./tools/index -tag v1.2.3 [-repo owner/repo] [-dist 目录] [-staging 目录] [-out 路径]

把已产出的插件二进制整理成发布资产名、算出 sha256，并写出 plugins/index.json。

选项：
  -tag TAG         发布 tag（必填，含前导 v；core.version 用它去掉 v 的值）
  -repo owner/repo 下载 URL 指向的仓库，默认 ` + defaultRepo + `
  -dist 目录       插件产物根目录，默认 internal/pluginbundle/dist
                   （即 go run ./tools/build plugins 的默认输出位置）
  -staging 目录    改名后的待上传资产目录，默认 dist/plugins
  -out 路径        index.json 路径，默认 <staging>/index.json

例：
  go run ./tools/index -tag v0.0.0-p3-smoke
`

const (
	// schemaVersion 是 index.json 的结构版本。字段只增不改；改形状必须 +1，
	// 否则旧客户端会把新字段当零值静默跳过。
	schemaVersion = 1

	defaultRepo    = "knight000/go_qqbot"
	defaultDist    = "internal/pluginbundle/dist"
	defaultStaging = "dist/plugins"

	// assetPrefix 是插件资产的文件名前缀，用来把插件产物与核心产物
	// （qqbot-<ver>-<os>-<arch>）区分开：两者同处一个 release 的资产列表。
	assetPrefix = "qqbot-plugin-"
)

// platform 是一个发布目标平台。index.json 的资产键是 <os>-<arch>，
// 产物目录是 <os>_<arch>——两种分隔符都是既有契约，不能统一。
type platform struct {
	os   string
	arch string
}

func (p platform) String() string { return p.os + "-" + p.arch }
func (p platform) dir() string    { return p.os + "_" + p.arch }

// releasePlatforms 是随仓库发布的平台，必须与 internal/pluginbundle 的三份
// //go:embed 目录一一对应：多一个平台没有产物、少一个平台漏发。
var releasePlatforms = []platform{
	{"windows", "amd64"},
	{"linux", "amd64"},
	{"linux", "arm64"},
}

// index 是 index.json 的根对象。
type index struct {
	Schema  int           `json:"schema"`
	Core    coreEntry     `json:"core"`
	Plugins []pluginEntry `json:"plugins"`
}

type coreEntry struct {
	Version string `json:"version"`
}

type pluginEntry struct {
	Name    string           `json:"name"`
	Version string           `json:"version"`
	Assets  map[string]asset `json:"assets"`
}

type asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// bundled 是随仓库发布的示例插件：名字与版本都来自插件的 Manifest()。
type bundled struct {
	name    string
	version string
}

// bundledPlugins 显式列出四个示例插件包。
//
// 显式列表而不是扫 plugins/ 目录：索引只该覆盖「随这个 release 发布的插件」，
// 开发工作区里的私有插件（plugins/<repo>/ 的嵌套克隆）不属于它。
func bundledPlugins() ([]bundled, error) {
	plugins := []host.Plugin{
		&echo.Plugin{},
		&adminping.Plugin{},
		&bootnotice.Plugin{},
		&md.Plugin{},
	}
	out := make([]bundled, 0, len(plugins))
	for _, p := range plugins {
		m := p.Manifest()
		if m.Name == "" || m.Version == "" {
			return nil, fmt.Errorf("插件 %T 的 Manifest 缺少 name 或 version", p)
		}
		out = append(out, bundled{name: m.Name, version: m.Version})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// options 是一次生成的全部输入。抽成结构体是为了让路径与命名可测。
type options struct {
	tag     string
	repo    string
	dist    string
	staging string
	out     string
}

func (o options) indexPath() string {
	if o.out != "" {
		return o.out
	}
	return filepath.Join(o.staging, "index.json")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "index:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(os.Stderr, usageText)
		return nil
	}
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	return generate(o)
}

func parseArgs(args []string) (options, error) {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tag := fs.String("tag", "", "发布 tag（含前导 v）")
	repo := fs.String("repo", defaultRepo, "下载 URL 指向的仓库 owner/repo")
	dist := fs.String("dist", defaultDist, "插件产物根目录")
	staging := fs.String("staging", defaultStaging, "待上传资产目录")
	out := fs.String("out", "", "index.json 路径，默认 <staging>/index.json")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("多余的参数: %v", fs.Args())
	}
	if strings.TrimSpace(*tag) == "" {
		return options{}, errors.New("缺少 -tag：下载 URL 与 core.version 都要用它（-h 看用法）")
	}
	return options{tag: *tag, repo: *repo, dist: *dist, staging: *staging, out: *out}, nil
}

// generate 写出 index.json，并把每个插件产物按发布资产名拷进 staging。
//
// 拷贝而不是原地改名：internal/pluginbundle/dist/ 是 go:embed 的输入，
// 改它的文件名会让下一次 `go build` 找不到文件。
func generate(o options) error {
	plugins, err := bundledPlugins()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.staging, 0o755); err != nil {
		return fmt.Errorf("创建资产目录 %s: %w", o.staging, err)
	}

	idx := index{
		Schema: schemaVersion,
		// 版本号只经 -ldflags -X .../buildinfo.Version 注入，值是去掉前导 v 的 tag
		// （ADR-0006 决策 9/11）：这里必须与二进制自己报告的版本逐字一致。
		Core: coreEntry{Version: strings.TrimPrefix(o.tag, "v")},
	}

	for _, p := range plugins {
		entry := pluginEntry{
			Name:    p.name,
			Version: p.version,
			Assets:  make(map[string]asset, len(releasePlatforms)),
		}
		for _, tgt := range releasePlatforms {
			src := filepath.Join(o.dist, tgt.dir(), p.name+exeSuffix(tgt.os))
			name := assetName(p.name, p.version, tgt)
			if _, err := os.Stat(src); err != nil {
				return fmt.Errorf(
					"缺少插件产物 %s（%s）：先跑 go run ./tools/build plugins -target %s/%s",
					p.name, src, tgt.os, tgt.arch)
			}
			sum, err := copyAndSum(src, filepath.Join(o.staging, name))
			if err != nil {
				return err
			}
			entry.Assets[tgt.String()] = asset{
				URL:    downloadURL(o.repo, o.tag, name),
				SHA256: sum,
			}
		}
		idx.Plugins = append(idx.Plugins, entry)
	}

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	path := o.indexPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 index 目录 %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("写 %s: %w", path, err)
	}
	fmt.Printf("index: %d 个插件 × %d 个平台 -> %s\n", len(idx.Plugins), len(releasePlatforms), path)
	return nil
}

// assetName 是插件产物在 release 资产列表里的名字。
//
// 与核心产物（qqbot-<ver>-<os>-<arch>）同一套形状：扁平、带版本，
// 一个 release 里两类资产靠前缀区分，人肉浏览和脚本都省事。
func assetName(plugin, version string, tgt platform) string {
	return assetPrefix + plugin + "-" + version + "-" + tgt.String() + exeSuffix(tgt.os)
}

// downloadURL 是 GitHub Release 的资产直链。owner/repo 可配是为了让镜像
// 场景能改指向（ADR-0006 决策 3 的 asset_base），尽管目前只有上游一处。
func downloadURL(repo, tag, asset string) string {
	return "https://github.com/" + repo + "/releases/download/" + tag + "/" + asset
}

func exeSuffix(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

// copyAndSum 边拷边算，返回目标文件的 sha256（小写十六进制）。
// 权限位固定 0755：产物是给人下载后直接跑的可执行文件。
func copyAndSum(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("读取产物 %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("写资产 %s: %w", dst, err)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		out.Close()
		return "", fmt.Errorf("拷贝 %s -> %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("关闭 %s: %w", dst, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

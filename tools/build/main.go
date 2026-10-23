// Command build 按固定顺序产出 qqbot 的构件。
//
// 两条命令，对应 ADR-0005 决策 11 的构建顺序：
//
//	go run ./tools/build plugins   # 先产出四个插件的独立可执行文件
//	go run ./tools/build core      # 再编核心（go:embed 依赖上一步的产物）
//
// `all` 等价于依次跑这两步。插件产物必须落在 internal/pluginbundle/dist/
// <goos>_<goarch>/：go:embed 不能引用包目录之外的文件，所以那个位置没有
// 可配置的余地（ADR-0005 决策 11 已按此更正）。
//
// 工具本身不依赖 make，Windows 与 Linux 上是同一条命令；-target 让 CI 能
// 在任意一台机器上交叉产出三个发布平台。
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const usageText = `用法：go run ./tools/build <plugins|core|all> [-target os/arch] [-o 路径]

  plugins  把每个插件的 cmd 入口编成独立产物
  core     编 ./cmd/bot
  all      先 plugins 再 core（核心的 go:embed 依赖插件产物）

选项：
  -target os/arch  目标平台，默认本机；有出厂副本的平台是 windows/amd64、
                   linux/amd64、linux/arm64
  -o 路径          plugins：插件产物目录，默认 internal/pluginbundle/dist/<os>_<arch>
                   core/all：核心产物路径，默认仓库根的 bot[.exe]；
                   给一个已存在的目录则在其下按平台补默认文件名。
                   all 的 -o 只作用于核心——插件必须写进 embed 目录，否则核心编不过。

例：
  go run ./tools/build all
  go run ./tools/build plugins -target linux/arm64
  go run ./tools/build core -target linux/amd64 -o dist/qqbot-linux-amd64
`

// distRoot 是插件产物的默认根目录，相对模块根。
// 它必须与 internal/pluginbundle 的 //go:embed 指令逐字一致。
const distRoot = "internal/pluginbundle/dist"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(os.Stderr, usageText)
		return nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := moduleRoot(cwd)
	if err != nil {
		return err
	}
	p, err := parseArgs(args)
	if err != nil {
		return err
	}

	switch p.cmd {
	case cmdPlugins:
		return buildPlugins(root, p)
	case cmdCore:
		return buildCore(root, p)
	case cmdAll:
		// 顺序不能反：核心的 //go:embed 在编译期就要看到 dist/ 里的文件。
		if err := buildPlugins(root, p); err != nil {
			return err
		}
		return buildCore(root, p)
	}
	return fmt.Errorf("未知子命令 %v（-h 看用法）", p.cmd)
}

type target struct {
	os   string
	arch string
}

func (t target) String() string { return t.os + "/" + t.arch }

func parseTarget(s string) (target, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return target{}, fmt.Errorf("-target 需要 os/arch（例如 linux/arm64），得到 %q", s)
	}
	return target{os: parts[0], arch: parts[1]}, nil
}

type command int

const (
	cmdPlugins command = iota + 1
	cmdCore
	cmdAll
)

func (c command) String() string {
	switch c {
	case cmdPlugins:
		return "plugins"
	case cmdCore:
		return "core"
	case cmdAll:
		return "all"
	}
	return "?"
}

// plan 是一次构建的全部输入。抽出来是为了让路径计算可测——产物位置错了
// 的表现是「go:embed 找不到文件」，那在编译期才炸，离写错的地方很远。
type plan struct {
	cmd    command
	target target
	out    string
}

func parseArgs(args []string) (plan, error) {
	if len(args) == 0 {
		return plan{}, errors.New("缺少子命令：plugins、core 或 all（-h 看用法）")
	}

	var cmd command
	switch args[0] {
	case "plugins":
		cmd = cmdPlugins
	case "core":
		cmd = cmdCore
	case "all":
		cmd = cmdAll
	default:
		return plan{}, fmt.Errorf("未知子命令 %q（可用：plugins、core、all）", args[0])
	}

	fs := flag.NewFlagSet("build "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tgt := fs.String("target", runtime.GOOS+"/"+runtime.GOARCH, "目标平台 os/arch")
	out := fs.String("o", "", "输出路径")
	if err := fs.Parse(args[1:]); err != nil {
		return plan{}, err
	}
	if fs.NArg() > 0 {
		return plan{}, fmt.Errorf("多余的参数: %v", fs.Args())
	}
	t, err := parseTarget(*tgt)
	if err != nil {
		return plan{}, err
	}
	return plan{cmd: cmd, target: t, out: *out}, nil
}

// pluginDir 返回插件产物目录。
//
// all 的 -o 属于核心：插件无论怎么传都必须写进 embed 目录，否则下一步
// `go build ./cmd/bot` 直接失败（而失败信息指向 embed 指令，不指向用户传的 -o）。
func (p plan) pluginDir(root string) string {
	if p.cmd != cmdAll && p.out != "" {
		return p.out
	}
	return filepath.Join(root, filepath.FromSlash(distRoot), p.target.os+"_"+p.target.arch)
}

// corePath 返回核心产物路径。
func (p plan) corePath(root string) string {
	name := "bot" + exeSuffix(p.target.os)
	if p.out == "" {
		return filepath.Join(root, name)
	}
	if info, err := os.Stat(p.out); err == nil && info.IsDir() {
		return filepath.Join(p.out, name)
	}
	return p.out
}

func exeSuffix(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

// pluginNames 返回 plugins/ 下每个有产物入口的插件名，按字典序。
//
// 入口是 plugins/<name>/cmd/<name>/main.go。目录里有 Go 源码却找不到入口
// 直接报错：那意味着"构建全绿、指令没反应"——旧的生成式装配正是栽在
// 这类静默缺失上。
func pluginNames(root string) ([]string, error) {
	base := filepath.Join(root, "plugins")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("读取插件目录 %s: %w", base, err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		entry := filepath.Join(base, name, "cmd", name, "main.go")
		if _, err := os.Stat(entry); err != nil {
			if hasGoFiles(filepath.Join(base, name)) {
				return nil, fmt.Errorf(
					"plugins/%s: 缺少 cmd/%s/main.go——插件没有产物入口，构建会静默漏掉它", name, name)
			}
			continue // 只有文档/资源的目录不是插件
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("%s 下没有可构建的插件", base)
	}
	return names, nil
}

func hasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			return true
		}
	}
	return false
}

func buildPlugins(root string, p plan) error {
	names, err := pluginNames(root)
	if err != nil {
		return err
	}
	dir := p.pluginDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建插件产物目录 %s: %w", dir, err)
	}
	for _, name := range names {
		out := filepath.Join(dir, name+exeSuffix(p.target.os))
		pkg := "./plugins/" + name + "/cmd/" + name
		if err := goBuild(root, p.target, pkg, out); err != nil {
			return err
		}
		fmt.Printf("build: 插件 %s -> %s\n", name, out)
	}
	return nil
}

func buildCore(root string, p plan) error {
	out := p.corePath(root)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("创建核心产物目录 %s: %w", filepath.Dir(out), err)
	}
	if err := goBuild(root, p.target, "./cmd/bot", out); err != nil {
		return err
	}
	fmt.Printf("build: 核心 -> %s\n", out)
	return nil
}

// goBuild 用固定的裁剪参数编一个产物：与发布用的命令逐字一致，
// 本地跑出来的东西不会比发布件多带调试信息。
func goBuild(root string, tgt target, pkg, out string) error {
	args := []string{"build", "-trimpath", "-ldflags", "-s -w", "-o", out, pkg}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = buildEnv(tgt)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build %s: %w", pkg, err)
	}
	return nil
}

// buildEnv 钉住目标平台并关掉 cgo：全仓依赖都是纯 Go（modernc.org/sqlite），
// 产物不该随构建机的 cgo 可用性变化；交叉编译也要求这么做。
func buildEnv(tgt target) []string {
	pinned := map[string]bool{"GOOS": true, "GOARCH": true, "CGO_ENABLED": true}
	out := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && pinned[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GOOS="+tgt.os, "GOARCH="+tgt.arch, "CGO_ENABLED=0")
}

// moduleRoot 从 dir 向上找第一个 go.mod。
func moduleRoot(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("找不到 go.mod：请在核心仓工作区内运行")
		}
		dir = parent
	}
}

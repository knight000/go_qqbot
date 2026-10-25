package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
)

const usageText = `用法：go run github.com/knight000/go_qqbot/tools/apisurface@<pin> [-dir 目录] [-check]

在插件 module 里跑一条命令，把该插件对契约成员的静态引用扫成成员级使用面
（"Event.Args"、"Params.Duration"），写到插件类型所在包里的 uses_gen.go。

<pin> 必须是本 module 的 go.mod 里 github.com/knight000/go_qqbot 的版本：
生成器要与它扫描的契约版本同源（ADR-0007 决策 4）。生成器会自己比对一次，
版本不一致直接失败。

选项：
  -dir 目录       插件 module 根目录，默认 .
  -check          只校验产物是否最新，不写文件（CI 用）
  -contract 路径  契约包导入路径，默认 ` + defaultContract + `
  -out 文件名     产物文件名（落在插件类型所在包），默认 uses_gen.go

例：
  go run github.com/knight000/go_qqbot/tools/apisurface@v0.0.2
  go run github.com/knight000/go_qqbot/tools/apisurface@v0.0.2 -check
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		// -h：flag 已经把用法打出来了，这不是错误。
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "apisurface:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("apisurface", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText) }

	var (
		dir      = fs.String("dir", ".", "插件 module 根目录")
		check    = fs.Bool("check", false, "只校验产物是否最新")
		contract = fs.String("contract", defaultContract, "契约包导入路径")
		outName  = fs.String("out", "uses_gen.go", "产物文件名")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("多余的参数 %v（选项要写在参数前？）", fs.Args())
	}

	s, err := scan(*dir, *contract, *outName)
	if err != nil {
		return err
	}

	gen := generatorVersion()
	if err := sameOrigin(gen, s.Contract); err != nil {
		return err
	}

	src, err := renderFile(emitData{
		Module:    s.Module,
		PkgName:   s.PkgName,
		TypeName:  s.TypeName,
		Pointer:   s.Pointer,
		Members:   s.Members,
		Contract:  s.Contract,
		Generator: gen,
	})
	if err != nil {
		return err
	}

	if *check {
		old, err := os.ReadFile(s.OutPath)
		if err != nil {
			return fmt.Errorf("产物缺失（%s）：先跑一次生成再提交", s.OutPath)
		}
		// 比对时抹掉「生成器」那一行：它记录的是**这次生成用的生成器从哪来**
		// （`@v0.0.2` 还是工作区），随调用方式变，不随插件代码变。不抹的话，
		// 从工作区生成的产物在 CI 里必然红——而 CI 的告警语义应当是
		// 「使用面过期了」，不是「你用了另一种构建方式」。
		// 「生成器必须与 pin 同源」这条另有 sameOrigin 专门把关，不靠这一行。
		if !bytes.Equal(stripGeneratorLine(old), stripGeneratorLine(src)) {
			return fmt.Errorf("%s 不是最新：重跑生成命令并提交（插件代码改了契约引用就会变）", s.OutPath)
		}
		fmt.Printf("使用面 %d 条，产物已是最新：%s\n", len(s.Members), s.OutPath)
		return nil
	}

	// 内容没变就不写：免得每次重跑都改 mtime，把构建缓存和 diff 都搅乱。
	// 这里比的是整份文件：生成器版本变了就该更新产物里的留痕。
	if old, err := os.ReadFile(s.OutPath); err == nil && bytes.Equal(old, src) {
		fmt.Printf("使用面 %d 条，无变化：%s\n", len(s.Members), s.OutPath)
		return nil
	}
	if err := os.WriteFile(s.OutPath, src, 0o644); err != nil {
		return fmt.Errorf("写产物 %s: %w", s.OutPath, err)
	}
	fmt.Printf("使用面 %d 条 → %s（契约 %s，生成器 %s）\n",
		len(s.Members), s.OutPath, s.Contract, gen)
	return nil
}

// renderFile 渲染并 gofmt。
//
// 走一遍 go/format 而不是靠手写的缩进对齐：产物要进 git，也要过
// `gofmt -l` 这类检查；生成器一旦吐出不合格式的文件，第一个红的是插件仓的
// CI，而它不会告诉作者「这是生成器的锅」。
func renderFile(d emitData) ([]byte, error) {
	src, err := format.Source(render(d))
	if err != nil {
		return nil, fmt.Errorf("产物格式化失败（生成器 bug）: %w", err)
	}
	return src, nil
}

// generatorVersion 是生成器自身的版本，写进产物头注释。
//
// 用 `@<pin>` 调用时这里是那个 pin（v0.0.1）；从工作区直接 `go run ./tools/...`
// 时是一串伪版本（v0.0.3-0.20261025043820-6c66bbf0af83+dirty）。(devel) 之外
// 的东西对读者没有信息量，统一写成「(工作区)」——顺带让没有 pin 的夹具仓
// 的产物保持逐字稳定（否则每换一次构建方式就产生一个无意义的 diff）。
//
// 它存在的意义是让「生成器与契约版本同源」这件事在产物里留痕：头注释里两个
// 版本号不一致，就是漂移的证据。
func generatorVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || !isPinnedVersion(bi.Main.Version) {
		return "(工作区)"
	}
	return bi.Main.Version
}

// sameOrigin 校验「生成器版本」与「插件 module 钉的契约版本」一致。
//
// ADR-0007 决策 4 要求生成器住在核心仓、插件用 `@<pin>` 调用，就是为了让
// 生成器理解它扫描的那一版契约。光靠约定不够：把 pin 写错一位、或者手滑跑了
// 另一版，产出的成员名可能对不上契约，而症状是「兼容判定永远不相交」——
// 放行一切。这里把它变成一次会失败的检查。
//
// 只在**生成器来自一个打好的版本**时检查。从工作区直接 `go build` 出来的是
// 伪版本（v0.0.3-0.20261025043820-6c66bbf+dirty）或 (devel)：那种情况下
// 生成器就是眼前这份工作区，与它扫的契约本来同源，没有可比的"钉住的那一版"。
func sameOrigin(gen string, o origin) error {
	if !isPinnedVersion(gen) || o.Version == "" || o.Version == gen {
		return nil
	}
	target := o.Version
	if o.Replaced != "" {
		target += "（被 replace 到 " + o.Replaced + "）"
	}
	return fmt.Errorf("生成器来自 %s，而本 module 钉的契约包是 %s："+
		"请用 `go run github.com/knight000/go_qqbot/tools/apisurface@%s` 生成"+
		"（生成器必须与它扫描的契约版本同源，ADR-0007 决策 4）",
		gen, target, o.Version)
}

// pseudoVersion 匹配 Go 的伪版本尾巴（`-0.20261025043820-6c66bbf0af83`）。
var pseudoVersion = regexp.MustCompile(`\d{14}-[0-9a-f]{12}$`)

// isPinnedVersion 判断版本号是不是一个**打出来的版本**（而不是工作区快照）。
func isPinnedVersion(v string) bool {
	switch {
	case !strings.HasPrefix(v, "v"):
		return false // (devel) / (unknown)
	case strings.Contains(v, "+"):
		return false // +dirty：工作区有未提交改动
	case pseudoVersion.MatchString(v):
		return false // 从某个 commit 编出来的，不对应任何 tag
	}
	return true
}

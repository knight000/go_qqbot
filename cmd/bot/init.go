package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/pluginbundle"
)

// defaultConfigPath 是 `-c` 的默认值：启动与 init 必须指向同一个文件，
// 否则用户 init 出来的配置不是下一次启动会读的那份。
const defaultConfigPath = "config/config.local.toml"

const initUsageText = `用法：qqbot init [-c 配置文件] [--plugins] [--force]

  -c 路径    配置文件路径（默认 ` + defaultConfigPath + `）
  --plugins  只释放缺失的出厂插件，不碰配置（删除即卸载，这是唯一的恢复路径）
  --force    覆盖已存在的配置文件（默认存在即报错）

默认行为：写配置 → 建数据目录 → 释放出厂插件到 <datadir>/plugins/bin/。
配置里的 [[bots]].appid 与 secret 留空，需要手工填入。
`

// runInit 实现 `qqbot init`，返回进程退出码：成功 0，失败非 0。
//
// 所有失败都必须留下可读的一句话：这条命令是新用户认识这个程序的第一面，
// 失败时只说"exit status 1"等于把他推回原处。
func runInit(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("init", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.Usage = func() { fmt.Fprint(stderr, initUsageText) }
	cfgPath := set.String("c", defaultConfigPath, "配置文件路径")
	pluginsOnly := set.Bool("plugins", false, "只释放缺失的出厂插件，不碰配置")
	force := set.Bool("force", false, "覆盖已存在的配置文件")
	if err := set.Parse(args); err != nil {
		// -h 是用户主动要的帮助，不是失败。
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if set.NArg() > 0 {
		fmt.Fprintf(stderr, "init: 多余的参数: %v\n", set.Args())
		return 2
	}

	if *pluginsOnly {
		return initPlugins(*cfgPath, stdout, stderr)
	}
	return initAll(*cfgPath, *force, stdout, stderr)
}

// initAll 是默认行为：写配置、建数据目录、释放出厂插件。
func initAll(path string, force bool, stdout, stderr io.Writer) int {
	if err := writeExampleConfig(path, force); err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}

	// 数据目录的口径与启动期逐字一致：filepath.Dir(cfg.Store.Path)
	// （cmd/bot/main.go 的 datadir）。走 LoadForInit 而不是手写默认值，
	// 是为了让"init 建出来的目录"永远等于"启动时去扫的目录"。
	cfg, err := config.LoadForInit(path)
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	datadir := filepath.Dir(cfg.Store.Path)
	if err := os.MkdirAll(datadir, 0o755); err != nil {
		fmt.Fprintf(stderr, "init: 创建数据目录 %s: %v\n", datadir, err)
		return 1
	}

	binDir := filepath.Join(datadir, "plugins", "bin")
	if err := pluginbundle.Extract(binDir); err != nil {
		// 配置已经写好了，别让用户以为整件事都没发生：说清楚哪一步成了、
		// 哪一步没成。空出厂副本（干净克隆）会走到这里。
		fmt.Fprintf(stderr, "init: 配置已写入 %s，但释放出厂插件失败: %v\n", path, err)
		return 1
	}

	fmt.Fprintf(stdout, "已生成配置：%s\n", path)
	fmt.Fprintf(stdout, "数据目录：%s\n", datadir)
	fmt.Fprintf(stdout, "出厂插件：%s → %s\n", strings.Join(pluginbundle.Names(), ", "), binDir)
	fmt.Fprintf(stdout, "\n下一步：\n")
	fmt.Fprintf(stdout, "  1. 编辑 %s，填入 [[bots]].appid 与 secret"+
		"（QQ 开放平台 → 机器人 → 开发设置）\n", path)
	fmt.Fprintf(stdout, "  2. 启动：qqbot -c %s\n", path)
	fmt.Fprintf(stdout, "\n以后要补回误删的插件，运行：qqbot init --plugins -c %s\n", path)
	return 0
}

// initPlugins 只释放缺失的插件，绝不碰配置文件（ADR-0005 决策 12）。
//
// 它需要 [store].path 才能定位数据目录，所以要求配置文件已经存在；
// 但不要求 app.id/secret 已填——那正是 init 生成配置后的常态。
func initPlugins(path string, stdout, stderr io.Writer) int {
	cfg, err := config.LoadForInit(path)
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	binDir := filepath.Join(filepath.Dir(cfg.Store.Path), "plugins", "bin")
	if err := pluginbundle.Extract(binDir); err != nil {
		fmt.Fprintln(stderr, "init: 释放出厂插件:", err)
		return 1
	}
	fmt.Fprintf(stdout, "已释放出厂插件：%s → %s（已存在的文件不动）\n",
		strings.Join(pluginbundle.Names(), ", "), binDir)
	return 0
}

// writeExampleConfig 把内嵌的示例配置写到 path，权限 0600（含 AppSecret）。
//
// 默认用 O_EXCL：已存在即报错，且报错里给出 --force 与 --plugins 两条出路。
// 用 O_EXCL 而不是先 Stat 再写，是为了让"并发两次 init"也只有一个人成功。
func writeExampleConfig(path string, force bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建配置目录 %s: %w", filepath.Dir(path), err)
	}
	flags := os.O_WRONLY | os.O_CREATE
	if force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("配置文件 %s 已存在，不覆盖"+
			"（要覆盖请加 --force；只补插件请用 --plugins）", path)
	}
	if err != nil {
		return fmt.Errorf("写配置 %s: %w", path, err)
	}

	_, err = f.Write(config.ExampleTOML())
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// 半截配置会被当成一份"填了一半的配置"：删掉，下次 re-init 才干净。
		// --force 时保留文件：那份文件可能是用户原来的配置，删掉比留个空壳更糟。
		if !force {
			_ = os.Remove(path)
		}
		return fmt.Errorf("写配置 %s: %w", path, err)
	}
	return nil
}

package pluginproc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/internal/core"
	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
	"github.com/knight000/go_qqbot/pkg/rpcsdk"
)

// defaultProbeTimeout 是 `bin --manifest` 的期限（ADR-0005 决策 5）。
//
// 2 秒不是拍脑袋：探测只做一次 JSON 编码 + 一次进程启动，正常在 50 ms 内；
// 留到 2 秒是因为冷启动遇上杀软扫描（Windows 首次读一个 5 MB 的新 exe）会
// 明显变慢，而探测阶段允许慢、不允许挂。挂住的插件必须被跳过，不能让整个
// 启动停在一个坏文件上。
const defaultProbeTimeout = 2 * time.Second

// Options 是装配期的输入。Dir 之外的零值都有可用默认。
type Options struct {
	// Dir 是插件目录，调用方给出 `<datadir>/plugins/bin`
	//（datadir = filepath.Dir(cfg.Store.Path)，这是全仓唯一口径）。
	Dir string
	// ConfigFor 返回某个插件的 `[plugins.<name>]` 原始配置，随 plugin.init
	// 交给插件。为 nil 时一律给空 map。
	ConfigFor func(name string) map[string]any
	// Disabled 是 `[plugins].disabled`：里面的名字一律不 spawn（决策 13）。
	Disabled []string
	// Registry 是错误码表。零值等价于 rpc.NewRegistry()。
	Registry *rpc.Registry
	Log      *slog.Logger
	// ProbeTimeout 覆盖 `--manifest` 的期限（测试用）。
	ProbeTimeout time.Duration
	// Backoff 覆盖崩溃退避表（默认 1/2/4/8/16 分钟；测试用短值）。
	Backoff []time.Duration
}

func (o Options) logger() *slog.Logger {
	if o.Log == nil {
		return slog.Default()
	}
	return o.Log
}

func (o Options) probeTimeout() time.Duration {
	if o.ProbeTimeout <= 0 {
		return defaultProbeTimeout
	}
	return o.ProbeTimeout
}

func (o Options) configFor(name string) map[string]any {
	if o.ConfigFor == nil {
		return map[string]any{}
	}
	cfg := o.ConfigFor(name)
	if cfg == nil {
		return map[string]any{}
	}
	return cfg
}

// Load 扫描插件目录，探测每个可执行文件的 manifest，返回可用的 proxy。
//
// 返回的切片按文件名排序（装配期日志要能两次启动逐字比对）。**任何单个插件
// 的问题都不返回 error**：目录里一个坏文件不该让机器人整体起不来（决策 6）。
// 只有"目录都读不了"这种整段装配无法继续的情况才返回 error。
//
// 目录不存在返回 (nil, nil)：还没跑过 `qqbot init` 是正常状态，不是故障
// （决策 12：文件在就是装着，不在就是没装）。
func Load(ctx context.Context, o Options) ([]host.Plugin, error) {
	log := o.logger()

	files, err := scan(o.Dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		log.Info("插件目录为空，未加载任何插件", "dir", o.Dir)
		return nil, nil
	}

	probes := probeAll(ctx, files, o.probeTimeout())
	// disabled 按 manifest 里的名字判定（文件名只是标签）：探测之后才知道
	// 每个文件自称是谁，所以这一步只能在这里做（ADR-0005「启动顺序」）。
	disabled := make(map[string]bool, len(o.Disabled))
	for _, name := range o.Disabled {
		disabled[name] = true
	}
	for i := range probes {
		if probes[i].err == nil && disabled[probes[i].manifest.Name] {
			probes[i].disabled = true
		}
	}

	out := make([]host.Plugin, 0, len(probes))
	// kept 记录已接受的插件名 → 文件：两个文件自称同一个名字（升级残留一个
	// 旧产物）时只能留一个——都装上会有两个进程抢同一条路由，表现为同一条
	// 消息回两遍，而日志里两个都"正常"。
	kept := make(map[string]string, len(probes))
	for _, pr := range probes {
		if pr.err != nil {
			// 跳过是刻意选择而不是兜底：启动日志里留一条 Error，落款写文件路径
			// ——没有这条，现场表现就是"某个指令没反应"且毫无线索（决策 6）。
			log.Error("插件探测失败，已跳过", "plugin", pr.name, "file", pr.file, "err", pr.err)
			continue
		}
		if pr.disabled {
			log.Info("插件在 [plugins].disabled 里，不启动", "plugin", pr.name, "file", pr.file)
			continue
		}
		p := newProxy(pr, o)
		if err := validateManifest(out, p); err != nil {
			log.Error("插件清单校验失败，已跳过", "plugin", pr.name, "file", pr.file, "err", err)
			continue
		}
		if first, dup := kept[pr.name]; dup {
			log.Error("插件重名，已跳过", "plugin", pr.name, "file", pr.file, "kept", first)
			continue
		}
		kept[pr.name] = pr.file
		log.Info("发现插件", "plugin", pr.name, "version", pr.manifest.Version,
			"api_version", pr.manifest.APIVersion, "commands", len(pr.manifest.Commands),
			"optional", len(pr.optional))
		out = append(out, p)
	}
	return out, nil
}

// validateManifest 用**核心自己的**路由构建器校验清单：api_version 一致、
// intents 可解析、每个命令恰好 Prefix 或 Pattern 之一、正则可编译、前缀不占
// 核心保留词、权限位合法（ADR-0005 决策 5「复用现有的全部启动期校验」）。
//
// 为什么绕一圈调 core.NewRouter 而不是在这里再写一遍：保留前缀表与 intent
// 语法都住在 core，抄一份到这里就必然有一天两边不一致——那一天的现场是
// "探测放过了，核心却拒绝"，而核心拒绝是**整体启动失败**。代价是这里多一个
// import 边（pluginproc → core），换来装配期校验只有一份实现。
//
// accepted 是已经通过校验的插件：把候选与它们一起构建，任何新错误都只能来自
// 候选本身。
func validateManifest(accepted []host.Plugin, cand host.Plugin) error {
	trial := make([]host.Plugin, 0, len(accepted)+1)
	trial = append(trial, accepted...)
	trial = append(trial, cand)
	_, err := core.NewRouter(trial)
	return err
}

// scan 列出目录里的普通文件，按文件名排序。
func scan(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取插件目录 %s: %w", dir, err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		// 只认普通文件：子目录（如 <name>/data）与符号链接都不该被当成插件。
		// 以 . 开头的（编辑器临时文件、.DS_Store）直接忽略。
		if !e.Type().IsRegular() || e.Name()[0] == '.' {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)
	return files, nil
}

// probeResult 是一次 `--manifest` 的结论。
type probeResult struct {
	file     string
	name     string // 文件名派生的兜底名字，只在探测失败时用于日志
	manifest host.Manifest
	optional []string
	disabled bool
	err      error
}

// probeAll 并行探测。并行是必须的：单看 2 秒超时不长，但串行跑五个插件，
// 一个坏文件就让启动多等 2 秒；而探测之间没有任何共享状态。
func probeAll(ctx context.Context, files []string, timeout time.Duration) []probeResult {
	results := make([]probeResult, len(files))
	var wg sync.WaitGroup
	for i, file := range files {
		wg.Add(1)
		go func(i int, file string) {
			defer wg.Done()
			manifest, optional, err := probeOne(ctx, file, timeout)
			name := filepath.Base(file)
			if err == nil && manifest.Name != "" {
				// 成功时按 manifest 的名字记日志：那是路由名，也是其它日志里
				// 出现的名字；文件名只是标签，不该在日志里抢戏（ADR-0005）。
				name = manifest.Name
			}
			results[i] = probeResult{
				file:     file,
				name:     name,
				manifest: manifest,
				optional: optional,
				err:      err,
			}
		}(i, file)
	}
	wg.Wait()
	return results
}

// probeOne 起 `bin --manifest` 并解析 stdout。
func probeOne(ctx context.Context, bin string, timeout time.Duration) (host.Manifest, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, rpcsdk.ManifestFlag)
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: 4 << 10}
	cmd.Stdout = &stdout
	// stderr 只留尾巴：探测失败的现场多数是"插件启动时打了段错误信息"，
	// 而一个坏插件可以打出几十 MB 日志——不能让它把启动内存吃掉。
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			// CommandContext 在期限到时会 Kill 掉它，不留下探测用的孤儿进程。
			return host.Manifest{}, nil, fmt.Errorf("--manifest 在 %s 内没有应答（已终止）", timeout)
		}
		return host.Manifest{}, nil, fmt.Errorf("执行 --manifest 失败: %w%s", err, stderr.tail())
	}
	return rpc.DecodeManifest(stdout.Bytes())
}

// tailBuffer 只保留最后 max 个字节，供错误串附带插件 stderr 的尾巴。
type tailBuffer struct {
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

// tail 把尾巴整理成"（stderr: …）"，空时返回空串（错误串不要以空括号结尾）。
func (b *tailBuffer) tail() string {
	if len(b.buf) == 0 {
		return ""
	}
	return fmt.Sprintf("（stderr: %s）", bytes.TrimSpace(b.buf))
}

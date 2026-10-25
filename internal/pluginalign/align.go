// Package pluginalign 在核心启动期用内嵌出厂副本对齐插件目录（#52）。
//
// 为什么需要它：内嵌副本只在内核编译期存在，插件升级走的是发布资产；换掉
// 核心 exe 再启动时，磁盘上的插件仍停在首次 `init --plugins` 的版本。而
// Extract 只补缺失、绝不覆盖（ADR-0005 决策 11），所以"换核心即升插件"
// 必须有一条独立的对齐路径。
//
// 三条边界：
//
//   - **只升不降**：判为兼容时磁盘版本更高就跳过（ADR-0006 决策 7），否则
//     用户单独升过的插件会被静默降级；
//   - **拒载优先**：磁盘插件被判拒载（host.CheckCompat 失败）时无条件替换，
//     不看版本号——那种插件根本加载不了，替换是唯一自愈路径，而内嵌副本按
//     本核心编译，不会同样被拒；
//   - **不阻断启动**：任何单个插件对齐失败都只 warn，机器人照常起来。
//
// 调用时机只有一个：pluginproc.Load 之前。那时插件进程还没起，文件可以
// 安全替换；之后替换正在被执行的二进制在 Windows 上必然失败。
package pluginalign

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/internal/pluginproc"
	"github.com/knight000/go_qqbot/internal/upgrade"
	"github.com/knight000/go_qqbot/pkg/host"
)

// defaultProbeTimeout 与装配期探测同一口径（ADR-0005 决策 5）：正常 50ms，
// 留到 2 秒是给冷启动遇上杀软扫描的余量。
const defaultProbeTimeout = 2 * time.Second

// Options 是一次对齐的全部输入。
type Options struct {
	// Dir 是插件目录 `<datadir>/plugins/bin`。
	Dir string
	// Bundle 是内嵌出厂副本（pluginbundle.Files()）。nil 或空时整轮不做事。
	Bundle fs.FS
	// Versions 是内嵌副本的版本清单（pluginbundle.Versions()），name → version。
	Versions map[string]string
	// Log 为 nil 时用 slog.Default()。
	Log *slog.Logger
	// Perm 是补回/替换后文件的权限位，默认 0755：插件是子进程可执行文件。
	Perm fs.FileMode
	// Probe 读取某个产物的清单，默认走 pluginproc.ProbeManifest。
	// 抽成字段是为了让判定矩阵的用例不必真的起进程。
	Probe func(ctx context.Context, bin string) (host.Manifest, error)
	// ProbeTimeout 覆盖默认探测期限（测试用）。
	ProbeTimeout time.Duration
}

// Report 是一次对齐的结论。四个切片里都是插件名（不是文件名）。
type Report struct {
	Added    []string // 缺失、从内嵌副本补回
	Replaced []string // 内嵌版本更高，或被判拒载
	Skipped  []string // 磁盘版本更高，或版本号无法比较
	Failed   []string // 写入失败（只 warn）
}

// Changed 返回真正动过磁盘的插件个数。
func (r Report) Changed() int { return len(r.Added) + len(r.Replaced) }

// Align 按判定矩阵对齐 Dir。任何单个插件的失败都只记 warn，不返回 error：
// 一条对齐失败的路不该让机器人下线。
func Align(ctx context.Context, o Options) Report {
	var rep Report
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	perm := o.Perm
	if perm == 0 {
		perm = 0o755
	}
	if o.Dir == "" || o.Bundle == nil || len(o.Versions) == 0 {
		return rep
	}
	probe := o.Probe
	if probe == nil {
		timeout := o.ProbeTimeout
		if timeout <= 0 {
			timeout = defaultProbeTimeout
		}
		probe = func(ctx context.Context, bin string) (host.Manifest, error) {
			return pluginproc.ProbeManifest(ctx, bin, timeout)
		}
	}

	disk := scanDisk(ctx, o.Dir, o.Versions, probe, log)

	for _, name := range sortedNames(o.Versions) {
		bundleVer := strings.TrimSpace(o.Versions[name])
		d, exists := disk[name]
		if !exists {
			target := filepath.Join(o.Dir, name+exeSuffix())
			if err := install(o.Bundle, name, target, perm); err != nil {
				log.Warn("补回出厂插件失败，跳过", "plugin", name, "path", target, "err", err)
				rep.Failed = append(rep.Failed, name)
				continue
			}
			log.Info("补回缺失的出厂插件", "plugin", name, "version", bundleVer, "path", target)
			rep.Added = append(rep.Added, name)
			continue
		}

		// 拒载优先于版本比较：被判拒载的插件连握手都过不去，留着它等于
		// 每次启动刷一条 Error。替换不设条件，连版本号不可比较也不管。
		if compatErr := host.CheckCompat(d.manifest.APIVersion, d.manifest.Uses); compatErr != nil {
			if err := install(o.Bundle, name, d.path, perm); err != nil {
				log.Warn("替换出厂插件失败，跳过", "plugin", name, "path", d.path, "err", err)
				rep.Failed = append(rep.Failed, name)
				continue
			}
			log.Info("替换出厂插件", "plugin", name,
				"from", d.manifest.Version, "to", bundleVer,
				"reason", "拒载："+compatErr.Error())
			rep.Replaced = append(rep.Replaced, name)
			continue
		}

		cmp, ok := upgrade.Compare(bundleVer, d.manifest.Version)
		switch {
		case !ok:
			log.Warn("插件版本号无法比较，跳过对齐",
				"plugin", name, "disk", d.manifest.Version, "bundle", bundleVer)
			rep.Skipped = append(rep.Skipped, name)
		case cmp > 0:
			if err := install(o.Bundle, name, d.path, perm); err != nil {
				log.Warn("替换出厂插件失败，跳过", "plugin", name, "path", d.path, "err", err)
				rep.Failed = append(rep.Failed, name)
				continue
			}
			log.Info("替换出厂插件", "plugin", name,
				"from", d.manifest.Version, "to", bundleVer, "reason", "内嵌版本更高")
			rep.Replaced = append(rep.Replaced, name)
		case cmp < 0:
			// 只升不降：用户单独升过的插件保持不动（ADR-0006 决策 7）。
			log.Info("磁盘插件版本更高，跳过对齐",
				"plugin", name, "disk", d.manifest.Version, "bundle", bundleVer)
			rep.Skipped = append(rep.Skipped, name)
		}
	}
	return rep
}

// diskPlugin 是磁盘上一个已探明身份的插件。
type diskPlugin struct {
	path     string
	manifest host.Manifest
}

// scanDisk 探测目录里**属于内嵌清单**的插件，返回 manifest 名 → 插件。
//
// 只探测清单里有的名字：第三方插件不归对齐管，多起一次进程只是白等；而
// 一个卡死的第三方插件会把整轮对齐拖到探测超时。点开头的文件一律不看：
// 那是落位文件（.echo.exe.new）与编辑器临时文件，pluginproc.Load 也这么忽略。
//
// 文件名只是标签，认身份最终靠 Manifest.Name：用户把 echo.exe 改名成
// myecho.exe 之后，按名字探测就找不到 echo、会补出第二份。所以有插件没对上
// 时（探测到的清单内插件数 < len(expected)）再拿目录里剩下的普通文件探一遍，
// 用自报的名字认领——这是兜底，全部对上时一个文件都不多探。
func scanDisk(ctx context.Context, dir string, expected map[string]string, probe func(context.Context, string) (host.Manifest, error), log *slog.Logger) map[string]diskPlugin {
	out := map[string]diskPlugin{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("读取插件目录失败，跳过对齐", "dir", dir, "err", err)
		}
		return out
	}
	var rest []fs.DirEntry
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, ok := expected[pluginName(e.Name())]; !ok {
			rest = append(rest, e)
			continue
		}
		path := filepath.Join(dir, e.Name())
		m, err := probe(ctx, path)
		if err != nil {
			// 探测不过的插件对齐也救不了：它的名字与版本都无从得知。
			// 保留原样，让 pluginproc.Load 照常记一条 Error。
			log.Warn("插件探测失败，跳过对齐", "file", path, "err", err)
			continue
		}
		if m.Name == "" {
			log.Warn("插件清单没有名字，跳过对齐", "file", path)
			continue
		}
		if first, dup := out[m.Name]; dup {
			log.Warn("多个文件自称同一个插件，只对齐第一个",
				"plugin", m.Name, "kept", first.path, "ignored", path)
			continue
		}
		out[m.Name] = diskPlugin{path: path, manifest: m}
	}
	if len(out) >= len(expected) {
		return out
	}
	// 还有插件没对上：可能只是没装，也可能是被改了名。逐个探剩余文件，只认
	// 清单里那些还没找到的名字。探测失败静默跳过——剩下的大多是第三方插件，
	// 为它们各起一次进程已经是代价，不该再刷一串 warn。
	for _, e := range rest {
		path := filepath.Join(dir, e.Name())
		m, err := probe(ctx, path)
		if err != nil || m.Name == "" {
			continue
		}
		if _, want := expected[m.Name]; !want {
			continue
		}
		if _, dup := out[m.Name]; dup {
			continue
		}
		log.Info("按插件自报的名字认出了改名后的出厂插件",
			"plugin", m.Name, "path", path)
		out[m.Name] = diskPlugin{path: path, manifest: m}
	}
	return out
}

// pluginName 从产物文件名去掉平台后缀。文件名只是标签，但它与内嵌清单的
// 键（Manifest().Name）在本仓是同一个值：tools/build 与 Extract 都按
// <name>[.exe] 落盘。
func pluginName(file string) string {
	return strings.TrimSuffix(file, exeSuffix())
}

// install 把内嵌副本里的某个插件写到 target。
//
// 走同目录临时文件 + rename（与 internal/upgrade/stage.go 同一口径）：目标
// 要么是旧文件、要么不存在，rename 保证读到的永远是完整的一份。临时文件
// 点开头，任何中途失败都不会被装配期当成插件。
func install(bundle fs.FS, name, target string, perm fs.FileMode) error {
	src, err := bundleFile(bundle, name)
	if err != nil {
		return err
	}
	data, err := fs.ReadFile(bundle, src)
	if err != nil {
		return fmt.Errorf("读取内嵌插件 %s: %w", src, err)
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建插件目录 %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".align-")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	// rename 成功后 Remove 只是返回 NotExist；失败路径上它负责清干净。
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写 %s: %w", tmpName, err)
	}
	// 先落盘再换名：掉电后 rename 指向的必须是一份完整内容。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭 %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("设置执行权限: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("替换 %s: %w", target, err)
	}
	return nil
}

// bundleFile 返回某个插件在内嵌副本里的文件名。
//
// 清单的键是插件名（Manifest().Name），产物文件名带平台后缀；两者只在
// Windows 上不同。先找带后缀的，再找不带后缀的，兼容测试夹具。
func bundleFile(bundle fs.FS, name string) (string, error) {
	for _, cand := range []string{name + exeSuffix(), name} {
		if _, err := fs.Stat(bundle, cand); err == nil {
			return cand, nil
		}
	}
	return "", fmt.Errorf("内嵌副本里没有插件 %s", name)
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
	"github.com/knight000/go_qqbot/pkg/rpcsdk"
)

// 本文件是插件升级的那一半（ADR-0006 决策 2/6/7）。
//
// 与核心升级共用下载、校验、落位、通道选择四件事，只有结尾不同：
// 核心要重启整个进程（换盘交给 main），插件只要重启那一个子进程，所以
// 校验不过、握手失败都能当场收住——这就是决策 6 说"回滚几乎免费"的形状。
//
// 两条路：
//
//	/upgrade plugin [name|all]   下载 → 校验 → 落位 → 验身份 → 换进程（直接生效）
//	/upgrade apply               下载 → 校验 → 落位 → 验身份 → 等 Commit 在关停后 rename
//
// 第二条只会替换**版本更高**的插件（决策 7）：内嵌副本是出厂态、index.json
// 是事实源，覆盖会把用户单独升过的插件静默降级。

// PluginReplacer 是一个插件进程代理暴露的两个能力。
//
// 只在方法集上对齐（pluginproc.proxy 实现），两个包不必互相 import：
// upgrade 不认识子进程，pluginproc 也不认识自更新。
type PluginReplacer interface {
	// BinPath 是当前插件可执行文件的绝对路径。落位文件必须与它同目录，
	// 否则 rename 会退化成跨卷的"复制 + 删除"。
	BinPath() string
	// ReplaceBinary 停旧进程、换上新产物、起新进程；失败时自己回滚。
	ReplaceBinary(newPath string) error
}

// PluginTarget 是一次插件升级的对象。
type PluginTarget struct {
	Name    string
	Version string // 已加载的版本
	Replace PluginReplacer
}

// PluginStage 是一份"已经落位、只等 rename"的插件产物。
//
// 它是 Apply 的产物、Commit 的输入：落位发生在进程还活着的时候，rename
// 只能发生在关停之后（决策 8 的顺序对插件同样成立——被替换的文件正在被子
// 进程执行）。
type PluginStage struct {
	Name    string
	From    string
	To      string
	Bin     string // 正式的插件文件路径
	NewPath string // 落位文件（与 Bin 同目录、点开头）
	Size    int64
}

// PluginReport 是一次插件升级的结论。单插件与 all 共用同一份渲染：
// 同一种行为只有一种说法。
type PluginReport struct {
	Tag      string
	Platform string
	Results  []PluginDelta
	Elapsed  time.Duration
}

// Changed 返回真正被替换掉的插件个数。
func (r PluginReport) Changed() int {
	n := 0
	for _, d := range r.Results {
		if d.Action == "升" {
			n++
		}
	}
	return n
}

// Text 是发给 owner 的那段话。
func (r PluginReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "插件升级（发布 %s｜%s）\n", r.Tag, r.Platform)
	if len(r.Results) == 0 {
		b.WriteString("没有要处理的插件。\n")
	}
	for _, d := range r.Results {
		fmt.Fprintf(&b, "  %s %s → %s  %s", d.Name, d.From, d.To, d.Action)
		if d.Size > 0 {
			fmt.Fprintf(&b, "（%s）", humanSize(d.Size))
		}
		b.WriteString("\n")
	}
	if r.Changed() == 0 {
		b.WriteString("没有插件被替换。\n")
	}
	fmt.Fprintf(&b, "耗时 %s", r.Elapsed.Round(time.Millisecond))
	return strings.TrimRight(b.String(), "\n")
}

// UpgradePlugins 依次升级给定的插件，逐条给出结论。
//
// 返回的 error 只表示"整条路走不通"（关掉自更新、版本是 dev、取不到索引）：
// 单个插件的失败与跳过都进报告——`all` 里一个插件坏掉不该让其余的一起失败。
//
// 顺序由 index.json 决定（不在索引里的排在最后）：两次执行同一批插件的
// 报告应当能逐行比对，顺序不能随 map 或入参漂移。
func (u *Upgrader) UpgradePlugins(ctx context.Context, targets []PluginTarget) (PluginReport, error) {
	rep := PluginReport{Platform: currentPlatform()}
	if !u.opt.Enabled {
		return rep, ErrDisabled
	}
	if err := u.guardVersion(); err != nil {
		return rep, err
	}
	if len(targets) == 0 {
		return rep, nil
	}
	if !u.applying.CompareAndSwap(false, true) {
		return rep, errors.New("已有一个升级在进行中，请稍后")
	}
	defer u.applying.Store(false)

	start := time.Now()
	rel, err := u.c.checkIndex(ctx)
	if err != nil {
		return rep, err
	}
	rep.Tag = rel.Tag
	for _, tgt := range orderByIndex(rel.Index, targets) {
		rep.Results = append(rep.Results, u.upgradePlugin(ctx, rel, tgt))
	}
	rep.Elapsed = time.Since(start)
	u.log.Warn("插件升级完成",
		"tag", rep.Tag, "targets", len(targets), "changed", rep.Changed(),
		"elapsed", rep.Elapsed.String())
	return rep, nil
}

// orderByIndex 按 index.json 的顺序排列目标；索引里没有的排在最后。
func orderByIndex(idx *Index, targets []PluginTarget) []PluginTarget {
	byName := make(map[string]PluginTarget, len(targets))
	for _, t := range targets {
		byName[t.Name] = t
	}
	out := make([]PluginTarget, 0, len(targets))
	for _, p := range idx.Plugins {
		if t, ok := byName[p.Name]; ok {
			out = append(out, t)
			delete(byName, p.Name)
		}
	}
	// 余下的按入参顺序追加：它们的结论都是"跳过"，顺序只影响阅读。
	for _, t := range targets {
		if _, ok := byName[t.Name]; ok {
			out = append(out, t)
			delete(byName, t.Name)
		}
	}
	return out
}

// upgradePlugin 处理单个插件，返回一条结论（不返回 error：失败也是一种结论）。
func (u *Upgrader) upgradePlugin(ctx context.Context, rel *Release, tgt PluginTarget) PluginDelta {
	d := PluginDelta{Name: tgt.Name, From: tgt.Version, To: tgt.Version}
	entry, ok := rel.Index.plugin(tgt.Name)
	if !ok {
		d.Action = "跳过（发布里没有）"
		return d
	}
	d.To = entry.Version
	asset, ok := entry.Assets[currentPlatform()]
	if !ok {
		d.Action = fmt.Sprintf("跳过（发布里没有 %s 的产物）", currentPlatform())
		return d
	}
	// 只升不降：同版本与本地更高都不动。措辞与预览里的 deltas 保持一致，
	// 同一件事在预览与执行结果里必须说同一句话。
	switch cmp, ok := Compare(tgt.Version, entry.Version); {
	case !ok:
		d.Action = "跳过（版本号无法比较）"
		return d
	case cmp > 0:
		d.Action = "跳过（比发布版本新）"
		return d
	case cmp == 0:
		d.Action = "跳过（已是最新）"
		return d
	}
	if tgt.Replace == nil {
		d.Action = "跳过（这个插件没有独立进程，不能热替换）"
		return d
	}
	bin := strings.TrimSpace(tgt.Replace.BinPath())
	if bin == "" {
		// 猜一个路径去下载是最坏的选择：换上去的可能是另一份产物。
		d.Action = "跳过（定位不到插件文件路径，无法落位）"
		return d
	}

	staged, err := u.stagePlugin(ctx, rel, tgt.Name, bin, entry.Version, asset)
	if err != nil {
		d.Action = "失败（" + err.Error() + "）"
		return d
	}
	if err := tgt.Replace.ReplaceBinary(staged.Path); err != nil {
		// 回滚是 ReplaceBinary 自己的事：它返回错误时旧版本已经在服务。
		// 落位文件在成功时已被 rename 走；失败时它可能还在，清掉。
		_ = os.Remove(staged.Path)
		d.Action = "失败（已回滚到 " + tgt.Version + "：" + err.Error() + "）"
		return d
	}
	d.Size = staged.Size
	d.Action = "升"
	return d
}

// stagedPlugin 是一份已下载、已校验、已落位、已验明正身的插件产物。
type stagedPlugin struct {
	Bin  string
	Path string
	Size int64
	SHA  string
}

// stagePlugin 把索引里的插件产物下载到插件同目录的点开头文件，校验 sha256、
// 落位（权限/fsync/文件头）、再起一次 `--manifest` 验明正身。
//
// 最后一步是这条路唯一"用真进程验证内容"的地方：sha256 保证字节没被动过，
// 但保证不了"索引里的版本号与产物自称的版本号一致"——CI 把两个平台的产物
// 搞混、发错产物，症状都是"装上去之后版本号没变"，而那时已经换完了。
//
// 任何一步失败都会删掉落位文件：留着它只会让下一次升级或 Commit 拿到一份
// 没人验过的半成品。
func (u *Upgrader) stagePlugin(ctx context.Context, rel *Release, name, bin, version string, asset Asset) (stagedPlugin, error) {
	if strings.TrimSpace(asset.SHA256) == "" {
		return stagedPlugin{}, fmt.Errorf("索引里没有 %s 的 sha256，拒绝下载", name)
	}
	url, err := u.c.assetURL(rel, asset.URL)
	if err != nil {
		return stagedPlugin{}, err
	}
	dst := PluginStagedPath(bin)
	staged, err := u.c.download(ctx, url, dst, asset.SHA256, MaxAssetBytes)
	if err != nil {
		return stagedPlugin{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(dst)
		}
	}()

	// 落位：权限、fsync、文件头（拒绝一份 200 OK 的错误页被当成产物）。
	if _, err := stageFile(bin, dst); err != nil {
		return stagedPlugin{}, err
	}
	// 验明正身：产物自己报一次身份，与索引逐字比对。
	m, err := probeManifest(ctx, dst, u.probeTimeout())
	if err != nil {
		return stagedPlugin{}, fmt.Errorf("新产物无法应答 %s: %w", rpcsdk.ManifestFlag, err)
	}
	if m.Name != name {
		return stagedPlugin{}, fmt.Errorf("产物自称插件 %q，索引里是 %q", m.Name, name)
	}
	if m.Version != version {
		return stagedPlugin{}, fmt.Errorf("产物自称版本 %q，索引里是 %q", m.Version, version)
	}
	ok = true
	return stagedPlugin{Bin: bin, Path: dst, Size: staged.Size, SHA: staged.SHA}, nil
}

// PluginStagedPath 是插件产物的落位路径：与插件同目录、点开头。
//
// 点开头是必须的，不是风格：pluginproc.Load 会把目录里每个非点开头的普通
// 文件都当成插件去探测，一份残留的落位文件会变成"插件重名"（两个文件自称
// 同一个名字），在下次启动时刷一条 Error——看起来像是升级把插件搞坏了。
func PluginStagedPath(bin string) string {
	return filepath.Join(filepath.Dir(bin), "."+filepath.Base(bin)+StagedSuffix)
}

// pluginOldPath 是插件换盘时的旧版本备份（Commit 用）。
func pluginOldPath(bin string) string {
	return filepath.Join(filepath.Dir(bin), "."+filepath.Base(bin)+OldSuffix)
}

// probeManifest 起一次 `<bin> --manifest`，把产物自称的身份读回来。
//
// 与 pluginproc 装配期那次探测是同一条协议（rpcsdk.ManifestFlag +
// rpc.DecodeManifest），但目的不同：这里不做插件发现，只问一个已知的产物
// "你是谁、哪一版"，所以不并行、也不"探测失败就跳过"——探测不过就是
// "这个产物不能装上"。
func probeManifest(ctx context.Context, bin string, timeout time.Duration) (host.Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, rpcsdk.ManifestFlag)
	var stdout strings.Builder
	var stderr tailBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			// CommandContext 在期限到时会 Kill 掉它，不留探测用的孤儿进程。
			return host.Manifest{}, fmt.Errorf("%s 在 %s 内没有应答（已终止）", rpcsdk.ManifestFlag, timeout)
		}
		return host.Manifest{}, fmt.Errorf("执行 %s 失败: %w%s", rpcsdk.ManifestFlag, err, stderr.tail())
	}
	m, _, err := rpc.DecodeManifest([]byte(stdout.String()))
	if err != nil {
		return host.Manifest{}, err
	}
	return m, nil
}

// tailBuffer 只保留最后 max 个字节，供错误串附带产物的 stderr 尾巴。
//
// 与 pluginproc 的同名类型是一个取舍：那里的是装配期（不该为探测失败多留
// 依赖），这里要的是"一句话说清产物为什么没起来"。
type tailBuffer struct {
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	if b.max == 0 {
		b.max = 4 << 10
	}
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) tail() string {
	if len(b.buf) == 0 {
		return ""
	}
	return fmt.Sprintf("（stderr: %s）", strings.TrimSpace(string(b.buf)))
}

// assetURL 把索引里给的产物地址换成"本次该走的那条通道"（决策 3/12）。
//
//   - 有 token：按资产名在发布资产列表里找 id，走 api_base 的资产端点——
//     私有仓的产物在镜像上根本取不到，而且 token 绝不能发给镜像主机；
//   - 没有 token：按 tag + 资产名拼 asset_base 的直链（可能是镜像）。
//
// 索引里的地址是生成时写死的 github.com 直链，不能直接拿去下载：那样
// asset_base 形同虚设。
func (c *client) assetURL(rel *Release, raw string) (string, error) {
	name := path.Base(strings.TrimSpace(raw))
	if name == "" || name == "." || name == "/" {
		return "", fmt.Errorf("索引里的产物地址不对: %q", raw)
	}
	if c.token != "" {
		id, _, ok := rel.gh.asset(name)
		if !ok {
			return "", fmt.Errorf("发布 %s 的资产列表里没有 %s：请确认 CI 上传了它", rel.Tag, name)
		}
		return c.apiAssetURL(id), nil
	}
	return c.mirrorURL(rel.Tag, name), nil
}

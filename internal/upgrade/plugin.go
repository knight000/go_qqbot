package upgrade

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
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
	// Upstream 是插件自己声明的发布源（owner/repo，见 Manifest.Upstream）。
	// 空 = 以核心仓的 index.json 为准（内置插件都是这种）。
	Upstream string
	Replace  PluginReplacer
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
//
// 头部只留平台：一条命令里可能同时有"核心仓的插件"和"自带上游的插件"，
// 统一写"发布 <tag>"会让后半截说错话。每条 delta 末尾单独写来源，
// 逐行可读、两次执行的报告也能逐行比对。
func (r PluginReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "插件升级（%s）\n", r.Platform)
	if len(r.Results) == 0 {
		b.WriteString("没有要处理的插件。\n")
	}
	for _, d := range r.Results {
		fmt.Fprintf(&b, "  %s %s → %s  %s", d.Name, d.From, d.To, d.Action)
		if d.Size > 0 {
			fmt.Fprintf(&b, "（%s）", humanSize(d.Size))
		}
		if d.Source != "" {
			fmt.Fprintf(&b, "（来源 %s）", d.Source)
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
// 顺序由 index.json 决定（不在索引里的按插件名排在最后）：两次执行同一批
// 插件的报告应当能逐行比对，顺序不能随 map 或入参漂移。
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

	// 只有没声明上游的插件才需要核心索引：一批全是上游插件时，核心仓一个
	// 请求都不发（#53）。取索引失败仍然整条失败——那是"核心这条路走不通"，
	// 与单个插件跳过不是一回事。
	var rel *Release
	if needsCoreIndex(targets) {
		var err error
		if rel, err = u.c.checkIndex(ctx); err != nil {
			return rep, err
		}
		rep.Tag = rel.Tag
	}
	var idx *Index
	if rel != nil {
		idx = rel.Index
	}
	for _, tgt := range orderByIndex(idx, targets) {
		rep.Results = append(rep.Results, u.upgradePlugin(ctx, rel, tgt))
	}
	rep.Elapsed = time.Since(start)
	u.log.Warn("插件升级完成",
		"tag", rep.Tag, "targets", len(targets), "changed", rep.Changed(),
		"elapsed", rep.Elapsed.String())
	return rep, nil
}

// needsCoreIndex 判断这批目标里有没有"以核心索引为来源"的插件。
func needsCoreIndex(targets []PluginTarget) bool {
	for _, t := range targets {
		if t.Upstream == "" {
			return true
		}
	}
	return false
}

// orderByIndex 排目标：核心索引里的按索引序在前，其余（自带上游的、索引里
// 没有的）按插件名升序在后。
//
// 索引可能为 nil（整批都是上游插件，没去取核心索引）。排序不能随入参漂移：
// 两次 /upgrade plugin all 的报告要能逐行比对。
func orderByIndex(idx *Index, targets []PluginTarget) []PluginTarget {
	byName := make(map[string]PluginTarget, len(targets))
	for _, t := range targets {
		byName[t.Name] = t
	}
	out := make([]PluginTarget, 0, len(targets))
	if idx != nil {
		for _, p := range idx.Plugins {
			if t, ok := byName[p.Name]; ok {
				out = append(out, t)
				delete(byName, p.Name)
			}
		}
	}
	rest := make([]PluginTarget, 0, len(byName))
	for _, t := range byName {
		rest = append(rest, t)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	return append(out, rest...)
}

// upgradePlugin 处理单个插件，返回一条结论（不返回 error：失败也是一种结论）。
//
// 一个插件只有一个来源：声明了上游就只查上游，查不到只跳过，绝不回落到
// 核心索引——否则一个拼错的上游会变成"看起来升级成功，其实版本没动"。
func (u *Upgrader) upgradePlugin(ctx context.Context, rel *Release, tgt PluginTarget) PluginDelta {
	d := PluginDelta{Name: tgt.Name, From: tgt.Version, To: tgt.Version}
	if tgt.Upstream != "" {
		return u.upgradePluginFromUpstream(ctx, tgt, d)
	}
	if rel == nil {
		// 有非上游目标时一定取过索引（needsCoreIndex），所以这里只是兜底：
		// 没有来源就不猜一个，如实说"没有声明发布来源"。
		d.Action = "跳过（没有声明发布来源）"
		return d
	}
	entry, ok := rel.Index.plugin(tgt.Name)
	if !ok {
		// 索引里没有它、自己也没声明上游：这个插件没有发布来源。说"发布里
		// 没有"会把原因指向核心仓的发布，而真实原因是它没声明去哪儿取。
		d.Action = "跳过（没有声明发布来源）"
		return d
	}
	// 来源只在"确实能从这个发布里取"时才写：先写来源再说没声明来源，
	// 报告会自相矛盾（"没有声明发布来源（来源 核心仓@tag）"）。
	d.Source = u.c.repo + "@" + rel.Tag
	d.To = entry.Version
	asset, ok := entry.Assets[currentPlatform()]
	if !ok {
		d.Action = fmt.Sprintf("跳过（发布里没有 %s 的产物）", currentPlatform())
		return d
	}
	// 只升不降：同版本与本地更高都不动。措辞与预览里的 deltas 保持一致，
	// 同一件事在预览与执行结果里必须说同一句话。
	bin, action := pluginInstallable(tgt, entry.Version, "跳过（比发布版本新）")
	if action != "" {
		d.Action = action
		return d
	}

	url, err := u.c.assetURL(ctx, rel, asset.URL)
	if err != nil {
		d.Action = "失败（" + err.Error() + "）"
		return d
	}
	return u.replaceWith(ctx, tgt, d, pluginArtifact{
		Name: tgt.Name, Bin: bin, Version: entry.Version,
		URL: url, SHA256: asset.SHA256, VersionFrom: "索引",
	})
}

// upgradePluginFromUpstream 查插件自己声明的发布源（#53）。
//
// 失败分类是这张票最容易做错的一处，逐条对着来：
//   - 不可达（404/403/429/超时/JSON 坏）→ 跳过（无法访问：<一句话>）：
//     `all` 里一个上游不可达不该让整条命令看起来失败；
//   - 没有本平台产物 / 没有校验和 / 不比磁盘新 → 跳过；
//   - sha256 不符、产物自称版本不符 → 失败：那是投毒或发错产物的信号。
func (u *Upgrader) upgradePluginFromUpstream(ctx context.Context, tgt PluginTarget, d PluginDelta) PluginDelta {
	up := tgt.Upstream
	if !validUpstream(up) {
		// 挡住"manifest 指定任意下载源"：含 URL、冒号、多段路径的一律无效。
		u.log.Warn("插件声明的上游发布源无效，跳过", "plugin", tgt.Name, "upstream", up)
		d.Action = "跳过（上游声明无效）"
		return d
	}
	// 先记仓、拿到 tag 再补 tag：不可达与"缺 tag"这两条结论也要能说清是
	// 哪个仓——它们是 owner 唯一能拿去查的线索。
	d.Source = up
	// 上游是**另一个仓**：同一个 tag 名在两个仓含义不同，必须按上游仓构造
	// client（自己的 repo、自己的 tag 缓存），不能复用核心仓那一个。
	cl := u.c.forRepo(up)
	rel, err := cl.latest(ctx)
	if err != nil {
		u.log.Warn("上游发布不可达，跳过", "plugin", tgt.Name, "upstream", up, "err", err)
		d.Action = "跳过（无法访问：" + accessReason(err) + "）"
		return d
	}
	tag := strings.TrimSpace(rel.TagName)
	if tag == "" {
		d.Action = "跳过（无法访问：最新发布缺少 tag）"
		return d
	}
	d.Source = up + "@" + tag

	asset, ok := upstreamPluginAsset(rel, tgt.Name)
	if !ok {
		d.Action = fmt.Sprintf("跳过（上游没有 %s 的产物）", currentPlatform())
		return d
	}
	d.To = asset.version
	// 与核心索引同一套口径：只升不降，同版本不动。
	bin, action := pluginInstallable(tgt, asset.version, "跳过（比上游版本新）")
	if action != "" {
		d.Action = action
		return d
	}
	sha, ok := digestSHA256(asset.digest)
	if !ok {
		// 没有校验和就没有可校验的哈希。stagePlugin 已经确立"没有 sha256 就
		// 不装"，这里不开第二条路（"没校验也装"）。
		d.Action = "跳过（上游没有校验和）"
		return d
	}

	// 地址永远由 api_base / asset_base + tag + 资产名拼出，不读 manifest 里的
	// 任何地址字段。这里把"上游仓的直链"当成索引里的那种地址交给 assetURL：
	// 通道选择（有 token 走 api 资产端点、没有走镜像直链）只该有一份实现。
	raw := cl.assetBase + "/" + up + "/releases/download/" + tag + "/" + asset.name
	url, err := cl.assetURL(ctx, &Release{Tag: tag, gh: rel}, raw)
	if err != nil {
		d.Action = "失败（" + err.Error() + "）"
		return d
	}
	return u.replaceWith(ctx, tgt, d, pluginArtifact{
		Name: tgt.Name, Bin: bin, Version: asset.version,
		URL: url, SHA256: sha, VersionFrom: "文件名",
	})
}

// pluginInstallable 判断一条候选产物"能不能装"，返回要落位的插件文件路径。
//
// 两条来源共用这一份判定：版本三分支（无法比较 / 本地更新 / 已是最新）、
// 有没有独立进程、定位不到文件，结论与措辞都必须一致。newer 是"本地比来源
// 更新"那一支的措辞——两条来源对"来源"的称呼不同（发布 / 上游），逐字沿用
// 各自的说法。action 非空表示跳过，此时 bin 无意义。
func pluginInstallable(tgt PluginTarget, to, newer string) (bin, action string) {
	switch cmp, ok := Compare(tgt.Version, to); {
	case !ok:
		return "", "跳过（版本号无法比较）"
	case cmp > 0:
		return "", newer
	case cmp == 0:
		return "", "跳过（已是最新）"
	}
	if tgt.Replace == nil {
		return "", "跳过（这个插件没有独立进程，不能热替换）"
	}
	bin = strings.TrimSpace(tgt.Replace.BinPath())
	if bin == "" {
		// 猜一个路径去下载是最坏的选择：换上去的可能是另一份产物。
		return "", "跳过（定位不到插件文件路径，无法落位）"
	}
	return bin, ""
}

// replaceWith 落位 → 验明正身 → 换进程，把结论写回 delta。
//
// 两条来源只有"取地址"那一段不同，剩下四步共用：同一件事只有一种实现，
// 也就只有一种失败文案。
func (u *Upgrader) replaceWith(ctx context.Context, tgt PluginTarget, d PluginDelta, art pluginArtifact) PluginDelta {
	staged, err := u.stagePlugin(ctx, art)
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

// validUpstream 判断 Manifest.Upstream 是否是唯一认的形状：owner/repo。
//
// 正则只放行这两段：含 ":"（URL / scheme）、含多段路径、含空白的一律无效。
// 另外挡掉 "." 与 ".."：它们能通过正则，却会让请求路径从 /repos/<owner>/<repo>
// 里跳出去（GitHub 侧会规范化路径），等于把"查哪个仓"换成"查同一主机的任意
// 路径"——manifest 不该有这种自由度。
var upstreamPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func validUpstream(s string) bool {
	if !upstreamPattern.MatchString(s) {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// validVersion 判断一个字符串是不是本仓认的版本号形状。
//
// 用它而不是 `Compare(s, s)`：拿比较器当校验器读起来像在比什么，实际只关心
// "解析得出来吗"。
func validVersion(s string) bool {
	_, ok := parseVersion(s)
	return ok
}

// digestSHA256 把 GitHub 资产的 digest（"sha256:<hex>"）拆成裸 hex。
//
// 只认 sha256：别的算法本包的下载校验算不出来，按"没有校验和"处理，
// 而不是"不校验也装"。
func digestSHA256(digest string) (string, bool) {
	const prefix = "sha256:"
	d := strings.TrimSpace(digest)
	if !strings.HasPrefix(d, prefix) {
		return "", false
	}
	sum := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(d, prefix)))
	if len(sum) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return "", false
	}
	return sum, true
}

// upstreamAsset 是上游发布里挑中的那一份产物。
type upstreamAsset struct {
	name    string
	version string
	digest  string
}

// upstreamPluginAsset 在上游最新发布的资产列表里挑出本平台的这个插件产物。
//
// 名字与 tools/index 同一套，不做第二套命名：
//
//	qqbot-plugin-<name>-<version>-<os>-<arch>[.exe]
//
// 上游发布不带 index.json，所以版本只能从文件名中间那段解析。同一份发布里
// 有多版时取最高的一版（CI 补发产物时的现场）。
func upstreamPluginAsset(rel *ghRelease, plugin string) (upstreamAsset, bool) {
	prefix := "qqbot-plugin-" + plugin + "-"
	suffix := "-" + currentPlatform()
	if runtime.GOOS == "windows" {
		suffix += ".exe"
	}
	var best upstreamAsset
	for _, a := range rel.Assets {
		if !strings.HasPrefix(a.Name, prefix) || !strings.HasSuffix(a.Name, suffix) {
			continue
		}
		ver := strings.TrimSuffix(strings.TrimPrefix(a.Name, prefix), suffix)
		if !validVersion(ver) {
			continue
		}
		if best.name != "" {
			if cmp, ok := Compare(ver, best.version); !ok || cmp <= 0 {
				continue
			}
		}
		best = upstreamAsset{name: a.Name, version: ver, digest: a.Digest}
	}
	return best, best.name != ""
}

// accessReason 把一次上游查询的失败压成一句话（失败分类表第一行）。
//
// 完整错误串会带 URL、可能还有换行；直接塞进报告会把其它条目淹掉。
func accessReason(err error) string {
	var he *httpError
	if errors.As(err, &he) {
		return he.status
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "超时"
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return oneLine(uerr.Err.Error())
	}
	return oneLine(err.Error())
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// stagedPlugin 是一份已下载、已校验、已落位、已验明正身的插件产物。
type stagedPlugin struct {
	Bin  string
	Path string
	Size int64
	SHA  string
}

// pluginArtifact 是一份"该装上去"的插件产物：装到哪、装的是哪一版、
// 字节从哪取、期望的 sha256 是多少。
//
// 核心索引与上游发布两条路都归一到这个形状，落位与验明正身因此只有一份
// 实现——同一件事只有一种说法。
type pluginArtifact struct {
	Name    string
	Bin     string
	Version string
	URL     string
	SHA256  string
	// VersionFrom 是版本号的出处（"索引" / "文件名"），只用于失败文案：
	// 产物自称与它对不上时，owner 要知道该去改哪一侧。
	VersionFrom string
}

// stagePlugin 把插件产物下载到插件同目录的点开头文件，校验 sha256、
// 落位（权限/fsync/文件头）、再起一次 `--manifest` 验明正身。
//
// 最后一步是这条路唯一"用真进程验证内容"的地方：sha256 保证字节没被动过，
// 但保证不了"期望的版本号与产物自称的版本号一致"——CI 把两个平台的产物
// 搞混、发错产物，症状都是"装上去之后版本号没变"，而那时已经换完了。
//
// 任何一步失败都会删掉落位文件：留着它只会让下一次升级或 Commit 拿到一份
// 没人验过的半成品。
func (u *Upgrader) stagePlugin(ctx context.Context, art pluginArtifact) (stagedPlugin, error) {
	if strings.TrimSpace(art.SHA256) == "" {
		return stagedPlugin{}, fmt.Errorf("索引里没有 %s 的 sha256，拒绝下载", art.Name)
	}
	dst := PluginStagedPath(art.Bin)
	staged, err := u.c.download(ctx, art.URL, dst, art.SHA256, MaxAssetBytes)
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
	if _, err := stageFile(art.Bin, dst); err != nil {
		return stagedPlugin{}, err
	}
	// 验明正身：产物自己报一次身份，与期望值逐字比对。
	m, err := probeManifest(ctx, dst, u.probeTimeout())
	if err != nil {
		return stagedPlugin{}, fmt.Errorf("新产物无法应答 %s: %w", rpcsdk.ManifestFlag, err)
	}
	if m.Name != art.Name {
		return stagedPlugin{}, fmt.Errorf("产物自称插件 %q，要装的是 %q", m.Name, art.Name)
	}
	if m.Version != art.Version {
		return stagedPlugin{}, fmt.Errorf("产物自称版本 %q，%s里是 %q", m.Version, art.VersionFrom, art.Version)
	}
	ok = true
	return stagedPlugin{Bin: art.Bin, Path: dst, Size: staged.Size, SHA: staged.SHA}, nil
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
//   - 有 token：按**URL 里自带的 tag** 取该 tag 的发布，再按资产名回查 id，
//     走 api_base 的资产端点——私有仓的产物在镜像上根本取不到，而且 token
//     绝不能发给镜像主机；
//   - 没有 token：按 tag + 资产名拼 asset_base 的直链（可能是镜像）。
//
// tag 必须从 URL 里取而不是用当前发布的 tag：没更新的插件会沿用上一条目，
// 它的地址指向上一版 tag（#52）。用当前 tag 重新拼会 404。
func (c *client) assetURL(ctx context.Context, rel *Release, raw string) (string, error) {
	tag, name := splitAssetURL(raw)
	if name == "" || name == "." || name == "/" {
		return "", fmt.Errorf("索引里的产物地址不对: %q", raw)
	}
	if tag == "" {
		tag = rel.Tag
	}
	if c.token == "" {
		return c.mirrorURL(tag, name), nil
	}
	gh := rel.gh
	if tag != rel.Tag {
		var err error
		if gh, err = c.releaseByTag(ctx, tag); err != nil {
			return "", err
		}
	}
	if gh == nil {
		return "", fmt.Errorf("没有发布 %s 的资产列表", tag)
	}
	id, _, ok := gh.asset(name)
	if !ok {
		return "", fmt.Errorf("发布 %s 的资产列表里没有 %s：请确认 CI 上传了它", tag, name)
	}
	return c.apiAssetURL(id), nil
}

// splitAssetURL 从索引里的直链取 (tag, 资产名)。
//
// 只认 tools/index 生成的那种形状：.../releases/download/<tag>/<asset>。
// 不是这个形状时返回空 tag，由调用方退回当前发布的 tag（与旧行为一致）。
func splitAssetURL(raw string) (tag, name string) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return "", path.Base(raw)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+3 < len(segs); i++ {
		if segs[i] == "releases" && segs[i+1] == "download" {
			return segs[i+2], segs[i+3]
		}
	}
	return "", path.Base(u.Path)
}

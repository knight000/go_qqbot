package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 本包把自更新的**可单测部分**收在一起：检查元数据、流式下载与校验、
// 落位、换盘与回滚、pending 状态机、库快照。进程替换本身不在这里——
// 它复用 /reload 已经做对的那条路（ADR-0006 决策 8）。
//
// 一次升级的时间轴（决策 2/5/6/8）：
//
//	/upgrade apply（进程还活着）
//	  快照 → 检查 → 下载 → 校验 → Stage(.new) → 写 pending 标记 → 回消息 → 计划交给 main
//	main（run 返回之后）
//	  Swap(.new → exe, 旧映像留 .old) → 重新执行自身
//	下一次启动（拿锁之前）
//	  读 pending：attempts+1；> 1 → Restore(.old) + 再重启一次
//	core.Start 完成且网关存活 30 秒
//	  删 pending 标记
//
// 这条轴的关键性质：**所有可能失败的动作都在进程还活着时做完**，关停之后
// 只剩两个 rename。任何一个 rename 失败都能在进程内收住（Swap 自带回滚），
// 而"新版本起来就崩"由 pending 标记兜底。

// ErrDisabled 表示配置里关掉了自更新。关掉之后一个请求都不发。
var ErrDisabled = errors.New("自更新已在配置里关闭（[upgrade].enabled = false）")

// ErrUpToDate 表示远端没有比当前更新的核心版本。
var ErrUpToDate = errors.New("当前已是最新版本")

// LoadedPlugin 是一个已装配插件的事实（名字 + 版本）。
//
// 只收"已加载"的插件：预览里的升/跳过清单比较的是 index.json 与这些
// manifest，磁盘上存在但被 [plugins].disabled 停用、或加载失败的插件
// 不参与——它们的版本号核心根本不知道。
type LoadedPlugin struct {
	Name    string
	Version string
}

// Options 是构造一次升级会话需要的全部输入。
type Options struct {
	// Enabled 对应 [upgrade].enabled，默认 true。
	Enabled bool
	// APIBase 是元数据通道（index.json 与 checksums.txt 永远走它）。
	APIBase string
	// AssetBase 是二进制通道（镜像），只在没有 token 时生效。
	AssetBase string
	// Token 是私有仓凭据。设了它三个产物全走 api_base。
	Token string
	// Repo 是 owner/repo。
	Repo string

	// Exe 是当前二进制的绝对路径，落位目标是它 + ".new"。
	Exe string
	// DataDir 是 datadir = filepath.Dir(cfg.Store.Path)，标记与快照落在它下面。
	DataDir string
	// Current 是当前版本（buildinfo.Version）。"dev" 直接拒绝（决策 9）。
	Current string
	// Plugins 是已加载插件，用于预览里的升/跳过清单。
	Plugins []LoadedPlugin
	// Snapshots 提供库快照；nil 表示这次装配没有数据库。
	Snapshots Snapshotter
	Log       *slog.Logger
	// Now 只给测试注入稳定时间；nil 用 time.Now。
	Now func() time.Time
}

// Upgrader 是 /upgrade 命令依赖的门面。
type Upgrader struct {
	opt Options
	c   *client
	log *slog.Logger
	now func() time.Time

	mu sync.Mutex
	// applying 挡住并发 apply。
	applying atomic.Bool
	// lastErr 是本次进程内最近一次失败的文案，供 /upgrade status 用。
	// 它刻意不落盘：写 state.json 的只有"已经开始换盘"这一件事，
	// 一次失败的预览/下载不该留下一个会被下一次启动误读的标记。
	lastErr error
}

// New 构造一个升级会话。它不发任何请求——请求只发生在 Preview/Apply 里。
func New(o Options) *Upgrader {
	logger := o.Log
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(o.APIBase) == "" {
		o.APIBase = DefaultAPIBase
	}
	if strings.TrimSpace(o.AssetBase) == "" {
		o.AssetBase = DefaultAssetBase
	}
	if strings.TrimSpace(o.Repo) == "" {
		o.Repo = DefaultRepo
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Upgrader{
		opt: o,
		c:   newClient(o.APIBase, o.AssetBase, o.Token, o.Repo),
		log: logger,
		now: now,
	}
}

// Report 是 `/upgrade`（预览）的内容。它不改任何文件，也不改任何状态。
type Report struct {
	Current  string
	Latest   string
	Tag      string
	Platform string
	CoreName string
	CoreSize int64
	UpToDate bool
	Plugins  []PluginDelta
}

// PluginDelta 是一个已加载插件相对最新发布的变化。
type PluginDelta struct {
	Name   string
	From   string // 已加载的版本
	To     string // 发布里的版本
	Action string // 升 / 跳过
}

// Preview 只读元数据：一次 releases/latest + 两份小清单，不碰 exe、
// 不碰 state.json、不下载二进制（验收 1 的前半）。
func (u *Upgrader) Preview(ctx context.Context) (Report, error) {
	if !u.opt.Enabled {
		return Report{}, ErrDisabled
	}
	if err := u.guardVersion(); err != nil {
		return Report{}, err
	}
	rel, err := u.c.check(ctx)
	if err != nil {
		return Report{}, err
	}

	rep := Report{
		Current:  u.opt.Current,
		Latest:   rel.Version,
		Tag:      rel.Tag,
		Platform: currentPlatform(),
		CoreName: rel.CoreName,
		CoreSize: rel.CoreSize,
		Plugins:  u.deltas(rel.Index),
	}
	if cmp, ok := Compare(u.opt.Current, rel.Version); ok && cmp >= 0 {
		rep.UpToDate = true
	}
	return rep, nil
}

// Text 是发给 owner 的那段话。
//
// 只列已加载的插件这条限制必须写在正文里：否则"某插件没出现在清单里"
// 会被读成"这个插件不需要升级"，而实际原因是它没被加载。
func (r Report) Text() string {
	var b strings.Builder
	if r.UpToDate {
		fmt.Fprintf(&b, "当前已是最新版本（%s，%s）。\n", r.Current, r.Platform)
		fmt.Fprintf(&b, "发布 %s 的核心产物 %s（%s）与本平台一致，无需升级。", r.Tag, r.CoreName, humanSize(r.CoreSize))
		return b.String()
	}
	b.WriteString("自更新预览（未改动任何文件）\n")
	fmt.Fprintf(&b, "当前 %s｜%s\n", r.Current, r.Platform)
	fmt.Fprintf(&b, "最新 %s（%s）\n", r.Latest, r.Tag)
	fmt.Fprintf(&b, "核心产物 %s（%s）\n", r.CoreName, humanSize(r.CoreSize))
	if len(r.Plugins) == 0 {
		b.WriteString("插件 无已加载插件\n")
	} else {
		b.WriteString("插件（只列已加载的插件）\n")
		for _, p := range r.Plugins {
			fmt.Fprintf(&b, "  %s %s → %s  %s\n", p.Name, p.From, p.To, p.Action)
		}
	}
	b.WriteString("用 /upgrade apply 应用核心升级：会先快照数据库，再重启进程。")
	return b.String()
}

// deltas 把 index.json 的插件版本与已加载的 manifest 逐一比较。
//
// 比较的是**已加载**的插件（ADR-0006 决策 7 的"只升不降"在 #45 落地时
// 用的也是这份比较）：比发布版本新的要跳过，否则会把用户单独升过的插件
// 静默降级。
func (u *Upgrader) deltas(idx *Index) []PluginDelta {
	out := make([]PluginDelta, 0, len(u.opt.Plugins))
	for _, p := range u.opt.Plugins {
		entry, _, ok := idx.lookup(p.Name)
		if !ok {
			out = append(out, PluginDelta{Name: p.Name, From: p.Version, To: p.Version, Action: "跳过（发布里没有）"})
			continue
		}
		d := PluginDelta{Name: p.Name, From: p.Version, To: entry.Version}
		switch cmp, ok := Compare(p.Version, entry.Version); {
		case !ok:
			d.Action = "跳过（版本号无法比较）"
		case cmp < 0:
			d.Action = "升"
		case cmp > 0:
			d.Action = "跳过（比发布版本新）"
		default:
			d.Action = "跳过（已是最新）"
		}
		out = append(out, d)
	}
	return out
}

// Plan 是一次已经准备好、只差换盘的升级。它由 Apply 产生，交给 main
// 在关停之后执行（决策 8：不新造重启路径，只给重启请求多带一份计划）。
type Plan struct {
	Exe      string
	NewPath  string
	From     string
	To       string
	Tag      string
	StateDir string
	Snapshot string
	Staged   int64
	Plugins  []PluginDelta
	At       time.Time
}

// Apply 做完全部"进程还活着时能做"的动作：快照 → 检查 → 下载 → 校验 → Stage，
// 最后写下 pending 标记。
//
// 任何一步失败都不会留下可被误读的状态：exe 不动、.new 删除、state.json 不写。
// 这正是验收 2 与验收 4 要求的行为，也是这套顺序存在的理由。
func (u *Upgrader) Apply(ctx context.Context) (*Plan, error) {
	// 同一时刻只允许一次升级：两个并发的 apply 会各自下载 40 MB、
	// 各自写 .new，第二个必然在校验或 rename 上失败，而失败现场会很难看。
	if !u.applying.CompareAndSwap(false, true) {
		return nil, errors.New("已有一个升级在进行中，请稍后")
	}
	defer u.applying.Store(false)

	p, err := u.apply(ctx)
	if err != nil {
		u.mu.Lock()
		u.lastErr = err
		u.mu.Unlock()
		return nil, err
	}
	return p, nil
}

func (u *Upgrader) apply(ctx context.Context) (*Plan, error) {
	if !u.opt.Enabled {
		return nil, ErrDisabled
	}
	if err := u.guardVersion(); err != nil {
		return nil, err
	}
	if u.opt.Exe == "" {
		return nil, errors.New("定位不到当前二进制的路径，无法升级")
	}
	if u.opt.DataDir == "" {
		return nil, errors.New("缺少数据目录，无法记录升级状态")
	}

	rel, err := u.c.check(ctx)
	if err != nil {
		return nil, err
	}
	if cmp, ok := Compare(u.opt.Current, rel.Version); ok && cmp >= 0 {
		return nil, ErrUpToDate
	}
	p := &Plan{
		Exe:      u.opt.Exe,
		NewPath:  StagedPath(u.opt.Exe),
		From:     u.opt.Current,
		To:       rel.Version,
		Tag:      rel.Tag,
		StateDir: StateDir(u.opt.DataDir),
		Plugins:  u.deltas(rel.Index),
		At:       u.now(),
	}

	// 1. 快照。必须早于任何落位动作：回滚二进制不等于回滚 schema。
	snap, err := TakeSnapshot(ctx, p.StateDir, u.opt.Snapshots, u.now())
	if err != nil {
		return nil, err
	}
	p.Snapshot = snap

	// 2. 清掉上一次的残渣，再下载。目标是 <exe>.new：与 exe 同目录 ⇒ 同卷。
	if err := RemoveStaged(u.opt.Exe); err != nil {
		return nil, fmt.Errorf("清理残留的 %s: %w", StagedPath(u.opt.Exe), err)
	}
	staged, err := u.c.download(ctx, rel.CoreURL, p.NewPath, rel.CoreSHA, MaxAssetBytes)
	if err != nil {
		return nil, err
	}

	// 3. 落位可行性（非空、可执行、同卷、fsync、0755）。失败在这里收住，
	//    进程继续服务，旧二进制与标记都不动。
	if _, err := Stage(u.opt.Exe); err != nil {
		return nil, err
	}
	p.Staged = staged.Size

	// 4. 写标记：从这一刻起，"换过盘"这件事在下一次启动时可见。
	if err := SaveState(p.StateDir, &State{
		Tag: p.Tag, From: p.From, To: p.To, Exe: p.Exe,
		Snapshot: p.Snapshot, Attempts: 0, StartedAt: p.At,
	}); err != nil {
		return nil, err
	}
	u.log.Warn("升级已就位，等待换盘",
		"from", p.From, "to", p.To, "exe", p.Exe,
		"bytes", p.Staged, "snapshot", p.Snapshot)
	return p, nil
}

// guardVersion 拦住所有"版本号不参与比较"的现场。
//
// dev 是本地 go build 的默认值（决策 9）：它既不能判断谁新谁旧，也不该
// 被 /upgrade "升"到某个正式版本。这条顺带挡住"本地构建混进生产"。
func (u *Upgrader) guardVersion() error {
	v := strings.TrimSpace(u.opt.Current)
	if v == "" || strings.EqualFold(v, "dev") {
		return fmt.Errorf("当前版本是 %q，无法参与版本比较，拒绝自更新：请用正式发布的产物运行（ADR-0006 决策 9）", v)
	}
	if _, ok := Compare(v, v); !ok {
		return fmt.Errorf("当前版本 %q 不是可比较的版本号，拒绝自更新", v)
	}
	return nil
}

// Status 是 `/upgrade status` 的内容。它只读本地状态，不发任何请求，
// 所以在 enabled=false 时仍然可用——关掉自更新只是关掉网络。
type Status struct {
	Current   string
	Platform  string
	Pending   bool
	State     *State
	LastError string
}

// Status 读 pending 标记与本次进程内的失败记录。
func (u *Upgrader) Status(context.Context) (Status, error) {
	st, err := LoadState(StateDir(u.opt.DataDir))
	u.mu.Lock()
	last := u.lastErr
	u.mu.Unlock()

	out := Status{Current: u.opt.Current, Platform: currentPlatform(), LastError: ""}
	if last != nil {
		out.LastError = last.Error()
	}
	if err != nil {
		// 标记坏了也要能回话：把错误放进 LastError，让 owner 看到具体原因。
		if out.LastError == "" {
			out.LastError = err.Error()
		}
		return out, nil
	}
	if st != nil {
		out.Pending = true
		out.State = st
	}
	return out, nil
}

// Text 是发给 owner 的那段话。
func (s Status) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "版本 %s｜%s\n", s.Current, s.Platform)
	if s.Pending {
		fmt.Fprintf(&b, "有待确认的升级：%s → %s（第 %d 次尝试启动）\n",
			s.State.From, s.State.To, s.State.Attempts)
		fmt.Fprintf(&b, "该标记在进程成功运行 30 秒后自动清除；再次启动仍未清除则回滚到 %s。\n", s.State.From)
	} else {
		b.WriteString("没有待确认的升级。\n")
	}
	if s.LastError != "" {
		fmt.Fprintf(&b, "上次失败：%s\n", s.LastError)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Commit 在关停之后换盘。
//
// 这是整条路径上唯一在"网关已断、库已关"之后发生的动作，所以它必须
// 要么成功、要么把旧二进制换回来——绝不允许留下一个没有 exe 的目录。
// 回滚成功时顺手清掉 pending 标记：既然回到了旧版本，就没有"未确认的升级"。
func Commit(p *Plan, log *slog.Logger) error {
	if p == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	if err := Swap(p.Exe); err != nil {
		// 换盘没成 → 旧二进制还在（Swap 自己会先试着换回来），
		// 所以"没有未确认的升级"：标记必须清掉，否则下一次启动会拿一份
		// 并不存在的升级去数尝试次数。
		rerr := Restore(p.Exe)
		_ = RemoveStaged(p.Exe)
		_ = ClearState(p.StateDir)
		if rerr != nil {
			return fmt.Errorf("换盘失败且回滚失败，二进制目录可能不可用: %w（回滚: %v）", err, rerr)
		}
		log.Error("换盘失败，已回滚到旧版本继续重启", "err", err, "exe", p.Exe)
		return err
	}
	log.Info("换盘完成，重启后由新版本接管",
		"from", p.From, "to", p.To, "exe", p.Exe, "old", OldPath(p.Exe))
	return nil
}

// humanSize 把字节数写成 MB（十进制，与 release 页面一致）。
func humanSize(n int64) string {
	if n <= 0 {
		return "大小未知"
	}
	if n < 1<<10 {
		return fmt.Sprintf("%d B", n)
	}
	const unit = 1024.0
	v := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB"} {
		v /= unit
		if v < unit || u == "GiB" {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%d B", n)
}

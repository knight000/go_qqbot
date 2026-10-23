package upgrade

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestUpgrader 装配一个把 exe/dataDir 都放进临时目录的会话。
func newTestUpgrader(t *testing.T, f *fakeRelease, current string, mutate func(*Options)) (*Upgrader, string) {
	t.Helper()
	exe := fakeExe(t)
	dataDir := t.TempDir()
	o := Options{
		Enabled:   true,
		APIBase:   f.api.URL,
		AssetBase: f.asset.URL,
		Repo:      "o/r",
		Exe:       exe,
		DataDir:   dataDir,
		Current:   current,
		Plugins: []LoadedPlugin{
			{Name: "echo", Version: "0.1.0"},
			{Name: "md", Version: "0.2.0"},
			{Name: "gone", Version: "0.1.0"},
		},
		Snapshots: &fakeSnapshotter{},
		// 发布口径是 2 秒（探测只做一次 JSON 编码 + 一次进程启动），但
		// `go test ./...` 会并行跑好几个包，冷启一个 5 MB 的产物超过 2 秒
		// 是常事——超时本身由 probeManifest 自己的用例盯着。
		ProbeTimeout: probeTimeoutForTest,
		Log:          slog.New(slog.DiscardHandler),
		Now:          func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local) },
	}
	if mutate != nil {
		mutate(&o)
	}
	return New(o), exe
}

// 验收 1 前半：预览只读元数据，不改任何文件。
// 断言的是"文件没被动过"这件事本身（大小 + mtime），而不是某个内部标志。
func TestPreviewChangesNoFiles(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("new"))
	u, exe := newTestUpgrader(t, f, "1.2.3", nil)

	before, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := u.Preview(context.Background())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if rep.Latest != "1.2.4" || rep.UpToDate {
		t.Fatalf("预览结论不对: %+v", rep)
	}
	if !strings.Contains(rep.Text(), "自更新预览") || !strings.Contains(rep.Text(), "1.2.4") {
		t.Errorf("预览正文不对:\n%s", rep.Text())
	}

	after, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Error("预览改动了二进制")
	}
	if _, err := os.Stat(StagedPath(exe)); !os.IsNotExist(err) {
		t.Error("预览不该产生 .new")
	}
	if _, err := os.Stat(filepath.Join(StateDir(t.TempDir()), stateFileName)); !os.IsNotExist(err) {
		t.Error("预览不该写标记")
	}
	if f.assetHits.Load() != 0 {
		t.Errorf("预览不该下载二进制，实际命中 %d", f.assetHits.Load())
	}
}

// 预览要给出插件升/跳过清单，并且明确写着"只列已加载的插件"。
func TestPreviewListsLoadedPluginDeltas(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("new"))
	f.index = `{"schema":1,"core":{"version":"1.2.4"},"plugins":[
	  {"name":"echo","version":"0.2.0","assets":{"` + currentPlatform() + `":{"url":"u","sha256":"s"}}},
	  {"name":"md","version":"0.1.0","assets":{"` + currentPlatform() + `":{"url":"u","sha256":"s"}}}
	]}`
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.Preview(context.Background())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	got := map[string]string{}
	for _, p := range rep.Plugins {
		got[p.Name] = p.Action
	}
	if got["echo"] != "升" {
		t.Errorf("echo 应判为升，实际 %q", got["echo"])
	}
	if !strings.HasPrefix(got["md"], "跳过") {
		t.Errorf("md（已加载版本更高）应跳过，实际 %q", got["md"])
	}
	if !strings.HasPrefix(got["gone"], "跳过") {
		t.Errorf("发布里没有的插件应跳过，实际 %q", got["gone"])
	}
	if !strings.Contains(rep.Text(), "只列已加载的插件") {
		t.Errorf("正文必须写明这条限制:\n%s", rep.Text())
	}
}

// 已经是最新版本时不提"应用升级"。
func TestPreviewReportsUpToDate(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", peHeader("same"))
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	rep, err := u.Preview(context.Background())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !rep.UpToDate {
		t.Fatalf("同版本应判为最新: %+v", rep)
	}
	if !strings.Contains(rep.Text(), "已是最新版本") {
		t.Errorf("正文不对:\n%s", rep.Text())
	}
}

// 决策 9：dev 不参与版本比较，直接拒绝——一个请求都不发。
func TestPreviewRejectsDevVersion(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", peHeader("x"))
	u, _ := newTestUpgrader(t, f, "dev", nil)

	_, err := u.Preview(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("应因 dev 拒绝，实际: %v", err)
	}
	if f.apiHits.Load() != 0 {
		t.Errorf("dev 拒绝时不该发请求，实际 %d 次", f.apiHits.Load())
	}
}

// 验收 5：[upgrade].enabled = false 时连检查都不发请求。
// 用一个"一旦被请求就记数并 500"的服务器断言零命中。
func TestDisabledIssueNoRequests(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("new"))
	f.broken.Store(true)
	u, exe := newTestUpgrader(t, f, "1.2.3", func(o *Options) { o.Enabled = false })

	if _, err := u.Preview(context.Background()); err != ErrDisabled {
		t.Fatalf("Preview 应返回 ErrDisabled，实际: %v", err)
	}
	if _, err := u.Apply(context.Background()); err != ErrDisabled {
		t.Fatalf("Apply 应返回 ErrDisabled，实际: %v", err)
	}
	if n := f.apiHits.Load(); n != 0 {
		t.Errorf("关掉自更新后不该发任何请求，实际 %d 次", n)
	}
	if _, err := os.Stat(StagedPath(exe)); !os.IsNotExist(err) {
		t.Error("关掉之后不该产生 .new")
	}
}

// 验收 1 后半（upgrade 侧）：apply 完成落位，给出换盘计划。
// 此时 exe 必须**还没**变——换盘发生在关停之后。
func TestApplyStagesAndWritesPending(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("new"))
	u, exe := newTestUpgrader(t, f, "1.2.3", nil)
	before := readFile(t, exe)

	plan, err := u.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if plan.To != "1.2.4" || plan.From != "1.2.3" || plan.Exe != exe {
		t.Fatalf("计划不对: %+v", plan)
	}
	if plan.NewPath != StagedPath(exe) {
		t.Errorf("落位路径 = %q", plan.NewPath)
	}
	if plan.Staged != int64(len(f.binary)) {
		t.Errorf("落位字节数 = %d want %d", plan.Staged, len(f.binary))
	}
	if plan.Snapshot == "" {
		t.Error("应拍一份库快照")
	}
	if _, err := os.Stat(plan.Snapshot); err != nil {
		t.Errorf("快照文件不存在: %v", err)
	}

	if got := readFile(t, exe); got != before {
		t.Error("apply 阶段不该改 exe（换盘在关停之后）")
	}
	if _, err := os.Stat(StagedPath(exe)); err != nil {
		t.Errorf(".new 应已就位: %v", err)
	}
	st, err := LoadState(plan.StateDir)
	if err != nil || st == nil {
		t.Fatalf("应写下 pending 标记: %v %v", st, err)
	}
	if st.To != "1.2.4" || st.From != "1.2.3" || st.Attempts != 0 {
		t.Errorf("标记内容不对: %+v", st)
	}

	// 换盘 + 下一次启动：attempts 0 → 1，不回滚。
	if err := Commit(plan, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := readFile(t, exe); !strings.HasSuffix(got, "new") {
		t.Error("Commit 之后 exe 应是新内容")
	}
	d, err := CheckPending(plan.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if d.Rollback {
		t.Fatalf("升级后的第一次启动不该回滚: %+v", d)
	}
}

// 验收 2：sha 不符 → 拒绝、临时文件删除、旧二进制与 state.json 都不动。
func TestApplyRejectsBadSHA(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("new"))
	f.sums = strings.Repeat("0", 64) + "  " + f.coreName + "\n"
	u, exe := newTestUpgrader(t, f, "1.2.3", nil)
	before := readFile(t, exe)

	if _, err := u.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "校验和不匹配") {
		t.Fatalf("应因校验和不匹配失败，实际: %v", err)
	}
	if _, err := os.Stat(StagedPath(exe)); !os.IsNotExist(err) {
		t.Error("临时文件必须删除")
	}
	if got := readFile(t, exe); got != before {
		t.Error("旧二进制被改动")
	}
	if st, err := LoadState(StateDir(filepath.Dir(exe))); err != nil || st != nil {
		t.Errorf("state.json 不该被写: %+v %v", st, err)
	}
}

// 验收 4（端到端）：下载成功但内容不可执行 → 进程内失败，服务继续。
// 旧 exe、state.json 都不动，也不留 .new。
func TestApplyRejectsNonExecutablePayload(t *testing.T) {
	body := []byte("<html>404 Not Found</html>")
	f := newFakeRelease(t, "1.2.4", body)
	f.sums = sha256Hex(body) + "  " + f.coreName + "\n"
	u, exe := newTestUpgrader(t, f, "1.2.3", nil)
	before := readFile(t, exe)

	_, err := u.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "不是可执行文件") {
		t.Fatalf("应因内容不可执行失败，实际: %v", err)
	}
	if _, err := os.Stat(StagedPath(exe)); !os.IsNotExist(err) {
		t.Error("失败后不该留 .new")
	}
	if got := readFile(t, exe); got != before {
		t.Error("旧二进制被改动")
	}
	if _, err := os.Stat(OldPath(exe)); !os.IsNotExist(err) {
		t.Error("不该产生 .old")
	}
	if st, err := LoadState(StateDir(u.opt.DataDir)); err != nil || st != nil {
		t.Errorf("state.json 不该被写: %+v %v", st, err)
	}
}

// 同版本再敲一次 apply：不下载、不落位，回一句"已是最新"。
func TestApplyRefusesWhenUpToDate(t *testing.T) {
	f := newFakeRelease(t, "1.2.3", peHeader("same"))
	u, exe := newTestUpgrader(t, f, "1.2.3", nil)

	if _, err := u.Apply(context.Background()); err != ErrUpToDate {
		t.Fatalf("应返回 ErrUpToDate，实际: %v", err)
	}
	if _, err := os.Stat(StagedPath(exe)); !os.IsNotExist(err) {
		t.Error("不该产生 .new")
	}
}

// 快照失败必须中止：没有库备份就换盘，跨破坏性迁移时回滚不了。
func TestApplyAbortsWhenSnapshotFails(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("new"))
	u, exe := newTestUpgrader(t, f, "1.2.3", func(o *Options) {
		o.Snapshots = &fakeSnapshotter{err: os.ErrPermission}
	})
	if _, err := u.Apply(context.Background()); err == nil {
		t.Fatal("快照失败应中止升级")
	}
	if _, err := os.Stat(StagedPath(exe)); !os.IsNotExist(err) {
		t.Error("不该开始下载")
	}
	if f.assetHits.Load() != 0 {
		t.Errorf("快照失败后不该下载，实际 %d", f.assetHits.Load())
	}
}

// status 读本地状态：没有标记时说"没有"，有标记时报版本与尝试次数。
func TestStatusReportsPending(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("new"))
	u, _ := newTestUpgrader(t, f, "1.2.3", nil)

	s, err := u.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.Pending || !strings.Contains(s.Text(), "没有待确认的升级") {
		t.Fatalf("初始状态不对: %+v", s)
	}

	plan, err := u.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	s, err = u.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Pending || !strings.Contains(s.Text(), "1.2.4") {
		t.Fatalf("apply 之后应报 pending: %+v", s.Text())
	}
	if plan.StateDir == "" {
		t.Error("计划应带上状态目录")
	}
}

// 换盘失败时必须把旧二进制换回来，绝不留下一个没有 exe 的目录。
// 用"把 .old 占成一个非空目录"制造 rename 失败。
func TestCommitRestoresOnSwapFailure(t *testing.T) {
	f := newFakeRelease(t, "1.2.4", peHeader("new"))
	u, exe := newTestUpgrader(t, f, "1.2.3", nil)
	before := readFile(t, exe)

	plan, err := u.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 占住 .old：Swap 清理不掉它，rename 就会失败。
	if err := os.MkdirAll(filepath.Join(OldPath(exe), "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(OldPath(exe), "sub", "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Commit(plan, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("换盘应失败")
	}
	if got := readFile(t, exe); got != before {
		t.Error("换盘失败后 exe 必须是旧内容")
	}
	// 回到旧版本就没有"未确认的升级"了。
	if st, err := LoadState(plan.StateDir); err != nil || st != nil {
		t.Errorf("回滚后应清掉标记: %+v %v", st, err)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:         "大小未知",
		512:       "512 B",
		1536:      "1.5 KiB",
		40 << 20:  "40.0 MiB",
		200 << 20: "200.0 MiB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q want %q", in, got, want)
		}
	}
}

package main

// 本文件是生成器的**防漏层**：扫描器静默漏一条，兼容判定就会放行一个真实的
// 不兼容变更，而症状是「加载成功、行为悄悄变了」。
//
// 所以这里不测「能跑」，测三件事：
//   - 三种咬过人的语法（泛型、嵌入接口、类型别名）各有一例，且每种都独占
//     某个成员名——产物里缺了那条就是那种写法没被扫到；
//   - 入仓的产物与「现在重新生成的结果」一致（生成器行为一改就红；只有
//     「生成器版本」那一行不比，它记录的是构建方式，不是内容）；
//   - 版本同源检查真的会失败（它不是装饰）。

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const fixtureDir = "testdata/fixture"

func TestScanFixture(t *testing.T) {
	s, err := scan(fixtureDir, defaultContract, "uses_gen.go")
	if err != nil {
		t.Fatalf("扫描夹具: %v", err)
	}

	// 产物落点：方法必须挂到 rpcsdk.Serve 收到的那个类型上，否则断言落空，
	// 使用面静默为空（= 退化为整体相等，机制白装）。
	if s.PkgName != "fixture" || s.TypeName != "Plugin" || !s.Pointer {
		t.Fatalf("插件类型 = %s.%s（指针 %v），期望 *fixture.Plugin", s.PkgName, s.TypeName, s.Pointer)
	}
	if !strings.HasSuffix(s.OutPath, filepath.Join("testdata", "fixture", "uses_gen.go")) {
		t.Fatalf("产物路径 = %s，期望落在插件类型所在的包", s.OutPath)
	}
	if s.Contract.APIVersion == "" {
		t.Fatal("没读出契约包的 api_version：产物头注释会缺少「对着哪一版」的证据")
	}

	// 票面上的三个真实引用例子。torn 那种规模的插件靠的就是这些成员级条目。
	for _, want := range []string{"Event.Args", "Params.Duration", "JobSpec"} {
		if !slices.Contains(s.Members, want) {
			t.Errorf("使用面缺 %q：夹具里明明引用了它", want)
		}
	}

	// 三种语法各一例。每条都是某个成员名的**唯一**来源，见夹具里的注释。
	cases := []struct{ member, why string }{
		{"Command.Pattern", "泛型：泛型函数返回值的字段"},
		{"AdminTarget", "泛型：泛型结构体的类型实参"},
		{"Host.Renderer", "嵌入接口：成员名取声明处，不取插件自己的接口"},
		{"Host.KV", "嵌入接口：同上"},
		{"Event.SenderID", "类型别名：别名用法要还原到 host.Event"},
	}
	for _, c := range cases {
		if !slices.Contains(s.Members, c.member) {
			t.Errorf("使用面缺 %q（%s）：这种写法被漏扫了", c.member, c.why)
		}
	}

	// 反向：不该出现「插件自己的类型名」这种在契约包里根本不存在的前缀。
	// 它一旦出现，兼容判定的求交就永远落空。
	for _, bogus := range []string{"surface.Renderer", "ev.SenderID", "pair.first"} {
		if slices.Contains(s.Members, bogus) {
			t.Errorf("使用面里出现了 %q：成员名必须是契约包里的声明处", bogus)
		}
	}
}

// TestCommittedArtifactIsUpToDate 是「产物入仓」这条验收项的守门人。
//
// 生成器的任何行为变化（多扫一条、少扫一条、顺序变了、头注释改了）都会让
// 这条红——那正是作者需要知道的事：插件的使用面要重新生成并 review。
func TestCommittedArtifactIsUpToDate(t *testing.T) {
	s, err := scan(fixtureDir, defaultContract, "uses_gen.go")
	if err != nil {
		t.Fatalf("扫描夹具: %v", err)
	}
	src, err := renderFile(emitData{
		Module:    s.Module,
		PkgName:   s.PkgName,
		TypeName:  s.TypeName,
		Pointer:   s.Pointer,
		Members:   s.Members,
		Contract:  s.Contract,
		Generator: generatorVersion(),
	})
	if err != nil {
		t.Fatalf("渲染产物: %v", err)
	}

	committed, err := os.ReadFile(s.OutPath)
	if err != nil {
		t.Fatalf("读入仓产物: %v", err)
	}
	if got, want := stripGeneratorLine(src), stripGeneratorLine(committed); !bytes.Equal(got, want) {
		t.Fatalf("入仓的 %s 与重新生成的结果不一致：重跑 `go run ./tools/apisurface -dir %s`\n--- 重新生成 ---\n%s",
			s.OutPath, fixtureDir, got)
	}
}

// TestCheckIgnoresGeneratorLine 锁住 `-check` 的口径：只对**内容**严格。
//
// 现场形态是这样的：作者在工作区用 `go run ./tools/apisurface` 生成，头注释
// 写「(工作区)」；CI 用 `go run ...@<pin> -check`，同一份产物重新渲染出的
// 头注释写 `v0.0.2`。两边使用面完全一致，只有那一行不同——它要是参与比对，
// CI 会永远红，而红的原因与「使用面过期」毫无关系。
func TestCheckIgnoresGeneratorLine(t *testing.T) {
	path := filepath.Join(fixtureDir, "uses_gen.go")
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读入仓产物: %v", err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			t.Fatalf("还原产物: %v", err)
		}
	})

	edited := make([]string, 0, len(orig))
	for _, line := range strings.Split(string(orig), "\n") {
		if strings.HasPrefix(line, generatorPrefix) {
			line = generatorPrefix + "v9.9.9"
		}
		edited = append(edited, line)
	}
	if err := os.WriteFile(path, []byte(strings.Join(edited, "\n")), 0o644); err != nil {
		t.Fatalf("改写产物: %v", err)
	}

	if err := run([]string{"-dir", fixtureDir, "-check"}); err != nil {
		t.Fatalf("只有生成器版本行不同，-check 却失败: %v", err)
	}
}

func TestCheckPassesOnFreshArtifact(t *testing.T) {
	if err := run([]string{"-dir", fixtureDir, "-check"}); err != nil {
		t.Fatalf("产物是最新的，-check 却失败: %v", err)
	}
}

func TestCheckDetectsStaleArtifact(t *testing.T) {
	path := filepath.Join(fixtureDir, "uses_gen.go")
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读入仓产物: %v", err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			t.Fatalf("还原产物: %v", err)
		}
	})
	// 模拟「改了插件代码但忘了重跑生成」：产物停在旧的那一份。
	if err := os.WriteFile(path, append(orig, []byte("\n// stale\n")...), 0o644); err != nil {
		t.Fatalf("改写产物: %v", err)
	}

	err = run([]string{"-dir", fixtureDir, "-check"})
	if err == nil || !strings.Contains(err.Error(), "不是最新") {
		t.Fatalf("-check 对过期产物返回 %v，期望报「不是最新」", err)
	}
}

// TestSameOrigin 覆盖「生成器版本由插件的 pin 决定」这条验收项。
func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name    string
		gen     string
		origin  origin
		wantErr string
	}{
		{
			name:   "与 pin 一致：放行",
			gen:    "v0.0.2",
			origin: origin{Version: "v0.0.2"},
		},
		{
			name:    "与 pin 不一致：失败并给出正确的 pin",
			gen:     "v0.0.2",
			origin:  origin{Version: "v0.0.1"},
			wantErr: "apisurface@v0.0.1",
		},
		{
			name:   "工作区伪版本：无从比较，跳过",
			gen:    "v0.0.3-0.20261025043820-6c66bbf0af83",
			origin: origin{Version: "v0.0.1"},
		},
		{
			name:   "工作区带未提交改动：跳过",
			gen:    "v0.0.2+dirty",
			origin: origin{Version: "v0.0.1"},
		},
		{
			name:   "(devel)：跳过",
			gen:    "(devel)",
			origin: origin{Version: "v0.0.1"},
		},
		{
			name:   "插件没钉版本（主 module）：跳过",
			gen:    "v0.0.2",
			origin: origin{Version: ""},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := sameOrigin(c.gen, c.origin)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("应放行，实际失败: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("错误 = %v，期望包含 %q", err, c.wantErr)
			}
		})
	}
}

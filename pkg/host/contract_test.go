package host

// 本文件是契约分级表的**防漏层**（ADR-0007 决策 11）。
//
// 能自动证明的做实，不能自动证明的留给评审：「这个成员名写对了吗」「版本号
// 有没有漏一版」机器能答；「这次改动到底算不算不兼容」机器答不了，靠
// contractChanges 上方的判据 + CI 的软校验（pkg/host 有 diff 而 contract.go
// 没有 diff 就告警）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestContractTableCoversEveryVersion 保证分级表覆盖 4~当前版本，且没有
// 漏版、倒序、或写一个还不存在的版本。
//
// 「涨版本忘了记一行」是这张表最可能的失效形态：漏的那一版等于给一个真实的
// 不兼容变更开后门，而且不会有任何现象——判定照常放行，直到线上出现一条
// 谁也解释不了的行为差异。
func TestContractTableCoversEveryVersion(t *testing.T) {
	cur, err := strconv.Atoi(APIVersion)
	if err != nil {
		t.Fatalf("host.APIVersion=%q 不是数字: %v", APIVersion, err)
	}

	// 表内按版本升序（同版本多行允许，v8 就有两档类别）。
	seen := make(map[int]bool, len(contractChanges))
	last := 0
	for i, ch := range contractChanges {
		if ch.Version < last {
			t.Fatalf("第 %d 行 v%d 排在 v%d 之后：分级表必须按版本升序", i, ch.Version, last)
		}
		last = ch.Version
		seen[ch.Version] = true
	}

	// 4~当前版本逐版都要有；3 及以前是线协议之前的历史，不在本表范围。
	for v := 4; v <= cur; v++ {
		if !seen[v] {
			t.Errorf("分级表缺 v%d：涨了版本就必须在 contractChanges 里记一行（判据见该变量上方注释）", v)
		}
	}
	if last != cur {
		t.Errorf("分级表最后一版是 v%d，而 host.APIVersion=%d：涨版本必须同时记表", last, cur)
	}
}

// TestContractTableVersionsAreUniquePerClass 允许同一版本多行，但同版本同类别
// 只该有一行——重复行不影响判定，只说明有人合并错了位置。
func TestContractTableVersionsAreUniquePerClass(t *testing.T) {
	type key struct {
		version int
		class   ChangeClass
	}
	seen := make(map[key]bool, len(contractChanges))
	for _, ch := range contractChanges {
		k := key{ch.Version, ch.Class}
		if seen[k] {
			t.Errorf("v%d 有重复的 %s 行：同一版本的同类变更应合并成一行", ch.Version, ch.Class)
		}
		seen[k] = true
	}
}

// TestContractTableClassesAreKnown 拦住手滑打错类别（"additve" 会让这条变更
// 从"跳过"变成"参与判定"，或反过来悄悄变成永远不判）。
func TestContractTableClassesAreKnown(t *testing.T) {
	for _, ch := range contractChanges {
		switch ch.Class {
		case Additive, Behavioral, Breaking:
		default:
			t.Errorf("v%d 的类别 %q 不是 additive/behavioral/breaking 之一", ch.Version, ch.Class)
		}
	}
}

// TestContractSurfaceMembersExist 是分级表的**硬校验**：每条 Surface 里的每个
// 成员名都必须真实存在于契约包。
//
// 拼错的成员名不会让任何东西编译失败，也不会让任何测试变红——它只会让那条
// 变更永远不与任何使用面相交，于是判定永远放行。这正是"漏一条等于给一个
// 真实的不兼容变更开后门"的具体形态，所以这里用 AST 扫本包源码取全集，
// 而不是维护第二份手写清单（那只是把拼错的机会从一处变两处）。
func TestContractSurfaceMembersExist(t *testing.T) {
	members := contractMembers(t)
	if len(members) == 0 {
		t.Fatal("没扫到任何契约成员：AST 扫描本身失效了")
	}

	for _, ch := range contractChanges {
		if ch.Surface == "*" {
			// 全局变更不点名成员，但只有 breaking 才配用 *：additive 用 *
			// 会被跳过（无意义），behavioral 用 * 等于把所有旧插件一刀切。
			if ch.Class != Breaking {
				t.Errorf("v%d 是 %s 却用了全局 \"*\"：只有破坏性变更可以一刀切", ch.Version, ch.Class)
			}
			continue
		}
		for _, member := range strings.Split(ch.Surface, ",") {
			member = strings.TrimSpace(member)
			if member == "" {
				t.Errorf("v%d 的 Surface %q 含空成员（多余的逗号？）", ch.Version, ch.Surface)
				continue
			}
			if !members[member] {
				t.Errorf("v%d 的成员 %q 在契约包里不存在：拼错的成员名会让这条变更永远判不到",
					ch.Version, member)
			}
		}
	}
}

// TestCheckCompat 是判定的行为矩阵，逐条对应 ADR-0007 决策 7 与 #55 的验收项。
func TestCheckCompat(t *testing.T) {
	cur, err := strconv.Atoi(APIVersion)
	if err != nil {
		t.Fatalf("host.APIVersion=%q 不是数字: %v", APIVersion, err)
	}
	prev := strconv.Itoa(cur - 1)

	cases := []struct {
		name    string
		built   string
		uses    []string
		wantErr string // 空串表示应放行
	}{
		{
			name:  "同版本无使用面：放行",
			built: APIVersion,
		},
		{
			name:  "同版本带使用面：放行",
			built: APIVersion,
			uses:  []string{"Event.Args"},
		},
		{
			// 整体相等的旧口径必须原样保留：没有使用面 = 依赖全部成员。
			name:    "上一版无使用面：拒载（与引入判定之前一致）",
			built:   prev,
			wantErr: "未声明使用面",
		},
		{
			// torn 的形状：按上一版编译、声明了使用面、且不含上一版新增的成员。
			name:  "上一版有使用面且不含新增成员：放行",
			built: prev,
			uses:  []string{"Event.Args", "Params.Duration", "JobSpec"},
		},
		{
			name:  "上一版有使用面但含新增成员：仍放行（纯增量，跳过）",
			built: prev,
			uses:  []string{"Manifest.Upstream"},
		},
		{
			// v10 的语义变更：用到 Params.Str 的旧插件必须被点名拒载。
			name:    "旧版用到 Params.Str：拒载且点名成员",
			built:   "9",
			uses:    []string{"Event.Args", "Params.Str"},
			wantErr: "Params.Str",
		},
		{
			// 同一次变更里的另一个成员同样要拒。
			name:    "旧版用到 QueryStore.Query：拒载且点名成员",
			built:   "9",
			uses:    []string{"QueryStore.Query"},
			wantErr: "QueryStore.Query",
		},
		{
			// v12 是全局变更，任何使用面都救不了按 11 及以前编译的插件：
			// 哪怕它一个变更成员都没碰到，也过不了 v12 那道墙。
			name:    "按 v9 编译且没碰 v10 的成员：仍被 v12 的全局变更拒载",
			built:   "9",
			uses:    []string{"Event.Args", "Host.KV"},
			wantErr: "全局变更",
		},
		{
			// v12 是全局变更，任何使用面都救不了按 11 及以前编译的插件。
			name:    "按 v11 编译：全局变更一律拒载",
			built:   "11",
			uses:    []string{"Event.Args"},
			wantErr: "全局变更",
		},
		{
			name:    "插件比核心新：拒载（向前兼容不做）",
			built:   strconv.Itoa(cur + 1),
			uses:    []string{"Event.Args"},
			wantErr: "向前兼容不做",
		},
		{
			name:    "api_version 不是版本号：拒载",
			built:   "v13",
			wantErr: "不是版本号",
		},
		{
			name:    "api_version 为空：拒载",
			built:   "",
			wantErr: "不是版本号",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckCompat(c.built, c.uses)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("应放行，实际拒载: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应拒载（含 %q），实际放行", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("拒载原因 = %q，期望包含 %q", err.Error(), c.wantErr)
			}
		})
	}
}

// contractMembers 扫本包（不含测试文件）的源码，返回「成员全集」。
//
// 成员名形状：
//   - "Type.Field"   结构体字段（JobSpec.PerPeer / Manifest.Upstream）
//   - "Type.Method"  接口方法或具名类型上的方法（Host.Renderer / Params.Str）
//   - "Name"         包级函数、变量、常量（ErrActiveQuota）
func contractMembers(t *testing.T) map[string]bool {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析契约包源码: %v", err)
	}

	out := make(map[string]bool)
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				switch decl := n.(type) {
				case *ast.TypeSpec:
					collectTypeMembers(decl, out)
				case *ast.FuncDecl:
					if recv := receiverName(decl.Recv); recv != "" {						out[recv+"."+decl.Name.Name] = true
					} else {
						out[decl.Name.Name] = true
					}
				case *ast.ValueSpec:
					for _, name := range decl.Names {
						out[name.Name] = true
					}
				}
				return true
			})
		}
	}
	return out
}

func collectTypeMembers(spec *ast.TypeSpec, out map[string]bool) {
	name := spec.Name.Name
	switch typ := spec.Type.(type) {
	case *ast.StructType:
		for _, f := range typ.Fields.List {
			for _, fieldName := range f.Names {
				out[name+"."+fieldName.Name] = true
			}
			// 内嵌字段没有名字，成员名就是被嵌入的类型名。
			if len(f.Names) == 0 {
				if embedded := receiverName(&ast.FieldList{List: []*ast.Field{f}}); embedded != "" {
					out[name+"."+embedded] = true
				}
			}
		}
	case *ast.InterfaceType:
		for _, m := range typ.Methods.List {
			for _, methodName := range m.Names {
				out[name+"."+methodName.Name] = true
			}
		}
	}
}

// receiverName 取接收者（或内嵌字段）的类型名，去掉指针与泛型实参。
func receiverName(list *ast.FieldList) string {
	if list == nil || len(list.List) == 0 {
		return ""
	}
	expr := list.List[0].Type
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.Ident:
			return e.Name
		case *ast.SelectorExpr:
			return e.Sel.Name
		default:
			return ""
		}
	}
}

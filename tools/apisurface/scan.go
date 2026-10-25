package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// 插件 module 里与本工具相关的两个固定导入路径。
const (
	// defaultContract 是契约包的导入路径，也是「成员名」这个命名空间的范围。
	defaultContract = "github.com/knight000/go_qqbot/pkg/host"
	// sdkPath 是插件入口所在的包：靠它定位插件类型（rpcsdk.Serve 的实参）。
	sdkPath = "github.com/knight000/go_qqbot/pkg/rpcsdk"
)

// loadMode 只要解析类型所需的那些：语法树、类型信息、导入图、module 版本。
//
// NeedDeps 不能省：没有它，依赖包只有名字没有 Types，`obj.Pkg().Path()`
// 这类判断会退化成「什么都匹配不上」，表现为「扫到 0 条」——一个不会报错的
// 静默漏扫。
const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedTypesInfo |
	packages.NeedSyntax | packages.NeedModule

// surface 是一次扫描的全部结论。
type surface struct {
	// Members 是排序去重后的成员名，形状与 contract.go 的 Surface 一致：
	// "Type.Field"（Event.Args）、"Type.Method"（Host.Renderer）、包级名字（JobSpec）。
	Members []string

	// 插件类型的位置：产物写进这个包，方法挂在这个类型上。
	Module   string
	PkgDir   string
	PkgName  string
	TypeName string
	// Pointer 表示 rpcsdk.Serve 收到的是 *T。接收者必须与它一致，否则
	// pkg/rpc 的类型断言（APISurface）落空，使用面静默变成空。
	Pointer bool

	// OutPath 是产物路径。
	OutPath string

	// Contract 是「这次扫描对着哪一版契约」的证据，写进产物头注释。
	Contract origin
}

// origin 描述插件 module 里解析出来的契约包版本。
type origin struct {
	Path string
	// Version 是契约包所在 module 的版本（v0.0.1）。本地 replace 时是
	// go.mod 里写的那个名义版本，Replaced 记录它被替换到了哪里。
	Version string
	// APIVersion 是契约包里的 host.APIVersion 常量（"12"），即兼容判定的锚点。
	APIVersion string
	// Replaced 是被 replace 的目标（本地路径或另一版），空表示没 replace。
	Replaced string
}

// String 是给头注释与日志用的一行摘要。
func (o origin) String() string {
	s := o.Path
	if o.Version != "" {
		s += " " + o.Version
	}
	if o.APIVersion != "" {
		s += "（api " + o.APIVersion + "）"
	}
	if o.Replaced != "" {
		s += " → " + o.Replaced
	}
	return s
}

// scan 在 dir（插件 module 根目录）里扫出使用面，并定位产物该写到哪。
//
// 扫的是 `./...`——整个 module，不只根包：插件的内部包（internal/xxx）
// 才是引用契约最多的地方，只看根包会漏掉大半。
func scan(dir, contractPath, outName string) (*surface, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("解析目录 %s: %w", dir, err)
	}

	pkgs, err := load(abs, contractPath)
	if err != nil {
		return nil, err
	}

	contract, err := findContract(pkgs, contractPath)
	if err != nil {
		return nil, err
	}

	owner, typeName, pointer, err := findPlugin(pkgs, contractPath)
	if err != nil {
		return nil, err
	}

	ownerDir, err := packageDir(owner)
	if err != nil {
		return nil, err
	}
	outPath := filepath.Join(ownerDir, outName)

	members, err := collect(pkgs, contract, outPath)
	if err != nil {
		return nil, err
	}

	return &surface{
		Members:  members,
		Module:   modulePath(owner),
		PkgDir:   ownerDir,
		PkgName:  owner.Name,
		TypeName: typeName,
		Pointer:  pointer,
		OutPath:  outPath,
		Contract: describeOrigin(contract),
	}, nil
}

// modulePath 取包所属 module 的路径，只用于产物头注释。
func modulePath(p *packages.Package) string {
	if p.Module != nil && p.Module.Path != "" {
		return p.Module.Path
	}
	return p.PkgPath
}

// load 载入 dir 下的全部包与契约包本身。
//
// 任何一个包类型检查失败都直接失败，不做「跳过坏的、扫好的」：残缺的包图
// 只会少扫，而少扫的表现是「插件被放行」——静默错行为，正是本工具要消灭的。
func load(dir, contractPath string) ([]*packages.Package, error) {
	cfg := &packages.Config{
		Mode:  loadMode,
		Dir:   dir,
		Tests: false, // 测试文件不是产物的一部分，不参与使用面
	}
	pkgs, err := packages.Load(cfg, "./...", contractPath)
	if err != nil {
		return nil, fmt.Errorf("加载 %s 的包: %w", dir, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("%s 里没有 Go 包：-dir 指到 module 根目录了吗", dir)
	}

	var errs []string
	for _, p := range pkgs {
		for _, e := range p.Errors {
			errs = append(errs, e.Error())
		}
	}
	if len(errs) > 0 {
		if len(errs) > 10 {
			errs = append(errs[:10], fmt.Sprintf("……另有 %d 条", len(errs)-10))
		}
		return nil, fmt.Errorf("包加载或类型检查失败（使用面不能基于残缺的包图生成，那会静默漏扫）:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return pkgs, nil
}

// findContract 从载入结果里取契约包。
func findContract(pkgs []*packages.Package, path string) (*packages.Package, error) {
	for _, p := range pkgs {
		if p.PkgPath == path && p.Types != nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("没有解析到契约包 %s：插件必须 import 它，否则没有任何使用面可扫", path)
}

// findPlugin 定位插件类型：找 `rpcsdk.Serve(<expr>)`，取 <expr> 的类型。
//
// 为什么靠这一处调用而不是猜「哪个类型实现了 Manifest()」：接口实现可以
// 藏在任何类型上（包装、嵌入、别名），而 Serve 的实参是**唯一**被真正交给
// 运行时的那个值。产物挂在别的类型上，断言必然落空，使用面静默为空。
func findPlugin(pkgs []*packages.Package, contractPath string) (*packages.Package, string, bool, error) {
	type hit struct {
		pkg      *packages.Package
		typeName string
		pointer  bool
	}
	hits := make(map[string]hit)

	for _, p := range pkgs {
		if p.PkgPath == contractPath || p.TypesInfo == nil {
			continue
		}
		for _, file := range p.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				fn, ok := p.TypesInfo.Uses[sel.Sel].(*types.Func)
				if !ok || fn.Pkg() == nil || fn.Pkg().Path() != sdkPath || fn.Name() != "Serve" {
					return true
				}
				typ := types.Unalias(p.TypesInfo.TypeOf(call.Args[0]))
				ptr := false
				if pt, ok := typ.(*types.Pointer); ok {
					ptr = true
					typ = types.Unalias(pt.Elem())
				}
				named, ok := typ.(*types.Named)
				if !ok || named.Obj().Pkg() == nil {
					return true
				}
				owner := findPackageByTypes(pkgs, named.Obj().Pkg())
				if owner == nil {
					return true
				}
				hits[owner.PkgPath+"."+named.Obj().Name()] = hit{owner, named.Obj().Name(), ptr}
				return true
			})
		}
	}

	switch len(hits) {
	case 0:
		return nil, "", false, errors.New(
			"没找到 rpcsdk.Serve(&Plugin{}) 调用：生成器靠它定位插件类型与产物落点" +
				"（入口通常是 cmd/<name>/main.go）")
	case 1:
		for _, h := range hits {
			return h.pkg, h.typeName, h.pointer, nil
		}
	}

	names := make([]string, 0, len(hits))
	for k := range hits {
		names = append(names, k)
	}
	sort.Strings(names)
	return nil, "", false, fmt.Errorf("找到 %d 处 rpcsdk.Serve 调用，无法确定插件类型：%s",
		len(hits), strings.Join(names, "、"))
}

// findPackageByTypes 用 *types.Package 的指针身份找回载入结果里的那个包。
func findPackageByTypes(pkgs []*packages.Package, tp *types.Package) *packages.Package {
	for _, p := range pkgs {
		if p.Types == tp {
			return p
		}
	}
	return nil
}

// packageDir 取包的目录。用 GoFiles 而不是 PkgPath 反推：cgo 与
// vendor/replace 场景下导入路径不等于磁盘位置。
func packageDir(p *packages.Package) (string, error) {
	for _, list := range [][]string{p.GoFiles, p.CompiledGoFiles} {
		if len(list) > 0 {
			return filepath.Dir(list[0]), nil
		}
	}
	return "", fmt.Errorf("包 %s 没有任何 Go 文件，无法确定产物落点", p.PkgPath)
}

// collect 汇总所有包里对契约成员的引用。
//
// 判据是「这个标识符解析到的对象，是不是契约包里的某个成员」——用 go/types
// 的对象身份，不是文本匹配。这一条同时解决三件难事：
//   - 类型别名（`type ev = host.Event`）自动还原到 host.Event，记的是 Event.Args；
//   - 泛型实参（`pick[host.Command]`）里的 host.Command 仍是 TypeName 对象；
//   - 嵌入接口（`type mine interface { host.Host }`）上调用方法时，对象是
//     真正声明它的那个接口里的方法（Host.Renderer），而不是 mine.Renderer。
func collect(pkgs []*packages.Package, contract *packages.Package, skipFile string) ([]string, error) {
	index := memberIndex(contract.Types.Scope())

	found := make(map[string]bool)
	for _, p := range pkgs {
		if p.TypesInfo == nil || p.Types == contract.Types {
			continue
		}
		for ident, obj := range p.TypesInfo.Uses {
			name, ok := index[obj]
			if !ok {
				continue
			}
			// 产物自己不算引用：它是纯数据，但万一有人往里加了 host.X，
			// 下一次生成就会把它算进去，于是每跑一次多一条。
			if skipFile != "" && p.Fset.Position(ident.Pos()).Filename == skipFile {
				continue
			}
			found[name] = true
		}
	}

	if len(found) == 0 {
		return nil, errors.New("没扫到任何契约成员：插件必然实现了 host.Plugin，" +
			"所以这一定是生成器失效或 -contract 指错了包")
	}

	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// memberIndex 把契约包里的每个成员映射到它的成员名。
//
// 成员名形状与 contract.go 的 Surface 逐字一致，因为两者要做字符串相交：
//   - "Type.Field"   结构体字段（含内嵌字段，内嵌字段名就是被嵌入的类型名）
//   - "Type.Method"  具名类型上的方法、接口里声明的方法
//   - "Name"         包级函数、变量、常量、类型
//
// 索引的是**对象指针**，所以查询时不需要知道调用点长什么样：字段选择、
// 复合字面量的键、方法调用、裸标识符，拿到的都是同一个对象。
func memberIndex(scope *types.Scope) map[types.Object]string {
	idx := make(map[types.Object]string, scope.Len()*4)
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		tn, ok := obj.(*types.TypeName)
		if !ok {
			idx[obj] = name
			continue
		}
		idx[obj] = name
		// 别名（type A = B）也要展开：宿主用别名时，引用的是别名那个对象，
		// 但它指向的成员在原始类型上。
		named, ok := types.Unalias(tn.Type()).(*types.Named)
		if !ok {
			continue
		}
		indexNamed(idx, name, named)
	}
	return idx
}

func indexNamed(idx map[types.Object]string, name string, named *types.Named) {
	for i := 0; i < named.NumMethods(); i++ {
		m := named.Method(i)
		idx[m] = name + "." + m.Name()
	}

	switch u := named.Underlying().(type) {
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			f := u.Field(i)
			idx[f] = name + "." + f.Name()
		}
	case *types.Interface:
		// 只索引**显式声明**在这一层的：内嵌接口的方法会随那个接口自己的
		// 索引项落位（它也是包级类型），两边名字因此都取「声明处」。
		for i := 0; i < u.NumExplicitMethods(); i++ {
			m := u.ExplicitMethod(i)
			idx[m] = name + "." + m.Name()
		}
	}
}

// describeOrigin 从契约包的 module 信息与常量里读出「对着哪一版」。
func describeOrigin(contract *packages.Package) origin {
	o := origin{Path: contract.PkgPath}
	if m := contract.Module; m != nil {
		o.Version = m.Version
		if m.Replace != nil {
			o.Replaced = m.Replace.Path
			if m.Replace.Version != "" {
				o.Replaced += " " + m.Replace.Version
			}
		}
	}
	// host.APIVersion 是字符串常量，取它的字面值就是兼容判定的锚点。
	if obj, ok := contract.Types.Scope().Lookup("APIVersion").(*types.Const); ok {
		o.APIVersion = strings.Trim(obj.Val().ExactString(), `"`)
	}
	return o
}

package host

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoModuleInternalImports 锁住契约包的依赖边界（ADR-0005 决策 1）。
//
// 「pkg/host 不得 import 任何实现包」目前只是验收命令里的一条 grep——
// 人一忙就会漏跑。P1 时期破坏它顶多是耦合，P3 之后它直接决定线协议里
// 会烧进什么：契约一旦依赖实现，跨进程格式就绑死了那个实现。
//
// 判据按模块前缀算而不是只看 `internal/`：`pkg/httpx` 这类包同样是实现，
// 换个目录放不该绕过这条约束。
func TestNoModuleInternalImports(t *testing.T) {
	const modulePrefix = "github.com/knight000/go_qqbot/"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取包目录: %v", err)
	}
	fset := token.NewFileSet()
	var checked int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s: %v", name, err)
		}
		checked++
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: 无法解析 import 路径 %s", name, spec.Path.Value)
			}
			if strings.HasPrefix(path, modulePrefix) {
				t.Errorf("%s 导入了本模块的实现包 %s：契约包只能依赖标准库与叶子依赖，"+
					"需要能力请在包内定义窄接口、由核心注入（ADR-0005 决策 1）", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有扫描到任何 Go 文件：目录不对，这条用例形同虚设")
	}
}

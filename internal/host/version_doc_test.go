package host

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestAPIVersionMatchesArchitectureTable 防止"改了常量、忘了加版本行"。
//
// `api_version` 是整体匹配，插件作者唯一能查到"这一版要跟着改什么"的地方
// 就是 ARCHITECTURE §3.2 的那张表。常量涨了而表没涨，等于让所有插件在
// "版本不匹配"的报错里自己猜原因。
func TestAPIVersionMatchesArchitectureTable(t *testing.T) {
	data, err := os.ReadFile("../../docs/ARCHITECTURE.md")
	if err != nil {
		t.Fatalf("读取 ARCHITECTURE.md: %v", err)
	}
	body := string(data)
	start := regexp.MustCompile(`契约版本历史`).FindStringIndex(body)
	if start == nil {
		t.Fatal("ARCHITECTURE.md 里找不到「契约版本历史」小节标题")
	}
	// 只看到下一个小标题为止：后面还有别的表，`| 100 |` 这类行会干扰。
	rest := body[start[1]:]
	if end := strings.Index(rest, "\n###"); end >= 0 {
		rest = rest[:end]
	}
	rows := regexp.MustCompile(`(?m)^\| (\d+) \|`).FindAllStringSubmatch(rest, -1)
	if len(rows) == 0 {
		t.Fatal("契约版本历史里没有任何版本行")
	}
	last := rows[len(rows)-1][1]
	if last != APIVersion {
		t.Fatalf("host.APIVersion = %q，但 ARCHITECTURE §3.2 表最后一版是 %q：涨版本必须在表尾补一行（写清插件要改什么）",
			APIVersion, last)
	}
}

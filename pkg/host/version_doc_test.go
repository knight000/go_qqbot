package host

import (
	"errors"
	"io/fs"
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
//
// 架构文档自 2026-10-24 起不入库（见 .gitignore 的 /docs/），干净克隆里没有
// 这份文件；此时跳过，而不是把"文档不在场"报成失败。
func TestAPIVersionMatchesArchitectureTable(t *testing.T) {
	data, err := os.ReadFile("../../docs/ARCHITECTURE.md")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("docs/ARCHITECTURE.md 不在场（架构文档不入库），跳过版本表一致性校验")
	}
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

// TestAPIVersionIsTwelve 把版本号钉成字面量。
//
// 表尾比对只能保证「表与常量一致」，两个一起忘了涨也会绿；这条用例要求
// 涨版本时必须同时想清楚「插件要改什么」——版本号就是这句话的声明。
//
// 7  引入 *HTTPStatusError；8 引入 Yield 取代 Broadcast、Event.Command/Args
// 与前缀词边界、Event.Mentioned / Raw（#11 / #12 / #13）；9 引入 Host.Session()
// 与 Event.Session（ADR-0004）；10 改了 Params.Str 的空串语义与 Store.Query
// 的只读实现（#30）；11 导出主动消息的三个失败哨兵（#34）；
// 12 把契约外置为 pkg/host，不匹配从编译错误变成握手拒绝（ADR-0005 / #38）。
func TestAPIVersionIsTwelve(t *testing.T) {
	if APIVersion != "12" {
		t.Fatalf("APIVersion=%q，期望 \"12\"", APIVersion)
	}
}

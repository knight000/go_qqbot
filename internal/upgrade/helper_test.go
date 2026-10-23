package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
	"github.com/knight000/go_qqbot/pkg/rpcsdk"
)

// sha256Hex 只给测试用：期望值必须来自独立来源（这里是对源字节现算），
// 不能让被测代码自己算一遍再自比。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fakeManifestTag 是"这份产物自称谁"在文件末尾的注入标记。
//
// 为什么编进内容：插件升级的最后一步是 rename，产物会改成插件的正式名字，
// 按文件名编码的身份在替换之后就退化了。为什么不直接写 JSON 到文件末尾再
// 让产物自己念——念的就是这段标记，格式由 rpc.EncodeManifest 生成，与本仓
// 唯一的线格式同源。
const fakeManifestTag = "FAKEMANIFEST:"

// TestMain 让测试二进制在带 `--manifest` 时扮演一个插件产物：把文件末尾
// 注入的那份清单原样打出来（rpcsdk 的探测契约）。
//
// 这是本包唯一"真进程"的夹具：插件升级要起 `bin --manifest` 验明正身，
// 用接口替身就把要验的事绕过去了；而每次 go build 一个夹具又要几秒。
func TestMain(m *testing.M) {
	if hasManifestFlag() {
		if line, ok := injectedManifest(); ok {
			fmt.Println(line)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func hasManifestFlag() bool {
	for _, a := range os.Args[1:] {
		if a == rpcsdk.ManifestFlag {
			return true
		}
	}
	return false
}

// injectedManifest 从自身可执行文件的尾部读回注入的清单。
//
// 只从文件尾找标记：常量本身也含这串字节（它在 rodata 里），从前往后找会
// 撞上它，而注入的那份一定是最后一段。
func injectedManifest() (string, bool) {
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	blob, err := os.ReadFile(self)
	if err != nil {
		return "", false
	}
	s := string(blob)
	i := strings.LastIndex(s, fakeManifestTag)
	if i < 0 {
		return "", false
	}
	line := strings.TrimSpace(s[i+len(fakeManifestTag):])
	if line == "" || !strings.HasPrefix(line, "{") {
		return "", false
	}
	return line, true
}

// fakePluginAsset 造一份"能通过 --manifest 探测"的插件产物：测试二进制 +
// 末尾注入的清单。返回的字节就是一次"发布"里的插件资产。
func fakePluginAsset(t *testing.T, name, version string) []byte {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制: %v", err)
	}
	blob, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("读取测试二进制: %v", err)
	}
	line, err := rpc.EncodeManifest(host.Manifest{
		Name:       name,
		Version:    version,
		APIVersion: host.APIVersion,
		Intents:    []string{"message"},
		Commands:   []host.Command{{Prefix: "/" + name}},
	}, []string{"plugin.start"})
	if err != nil {
		t.Fatalf("编码清单: %v", err)
	}
	out := append([]byte{}, blob...)
	out = append(out, []byte("\n"+fakeManifestTag+string(line)+"\n")...)
	return out
}

// fakePluginBin 在 dir 下造一个"已装好的插件"：文件名是插件的正式名字
// （与 tools/build、pluginbundle 的约定一致），内容由 fakePluginAsset 生成。
func fakePluginBin(t *testing.T, dir, name, version string) string {
	t.Helper()
	bin := filepath.Join(dir, name+exeSuffix())
	if err := os.WriteFile(bin, fakePluginAsset(t, name, version), 0o755); err != nil {
		t.Fatalf("写假插件: %v", err)
	}
	return bin
}

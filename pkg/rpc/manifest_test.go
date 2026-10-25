package rpc

import (
	"encoding/json"
	"strings"
	"testing"
)

// wireManifest 是 `bin --manifest` 与 plugin.manifest 应答**同一份**线格式的
// 事实源。它是手写的字面量而不是由 EncodeManifest 生成：这些字段名一旦漂移，
// 核心侧就读到零值（表现为"插件装上了但一条命令都不响应"，且没有任何报错），
// 所以必须有一个独立于代码的期望值来钉住它们。
//
// 改这个字面量等于改协议——两端要一起升级。
const wireManifest = `{
  "Manifest": {
    "Name": "echo",
    "Version": "1.2.3",
    "APIVersion": "12",
    "Uses": ["Event.Args", "Params.Duration"],
    "Capabilities": ["demo"],
    "Intents": ["message", "group.notice"],
    "Commands": [
      {"Prefix": "/echo", "Loose": true, "Priority": 5, "Description": "回显",
       "Permission": 4, "DenyReply": "不行"},
      {"Pattern": "^/re .*"}
    ],
    "Yield": true,
    "AllowedDomains": ["api.example.com"],
    "ConfigSchema": {"greeting": "string", "n": "int"},
    "Permission": 2,
    "DenyReply": "默认拒绝"
  },
  "Optional": ["plugin.start", "plugin.close", "plugin.run_job"]
}`

// DecodeManifest 必须吃得下 `--manifest` 的输出：字段名、嵌套命令、
// 可选方法表都对得上，不能有任何一个字段静默落成零值。
func TestDecodeManifestWireFormat(t *testing.T) {
	m, optional, err := DecodeManifest([]byte(wireManifest))
	if err != nil {
		t.Fatalf("解析清单: %v", err)
	}

	checks := []struct {
		what string
		got  any
		want any
	}{
		{"Name", m.Name, "echo"},
		{"Version", m.Version, "1.2.3"},
		{"APIVersion", m.APIVersion, "12"},
		// 使用面必须能穿过线协议：核心的兼容判定读的就是这一份，字段名漂了
		// 就退化成"所有插件都没声明使用面"，判定悄悄回到整体相等。
		{"Uses", strings.Join(m.Uses, ","), "Event.Args,Params.Duration"},
		{"Yield", m.Yield, true},
		{"Permission", int(m.Permission), 2},
		{"DenyReply", m.DenyReply, "默认拒绝"},
		{"Capabilities", len(m.Capabilities), 1},
		{"Intents[1]", m.Intents[1], "group.notice"},
		{"AllowedDomains[0]", m.AllowedDomains[0], "api.example.com"},
		{"ConfigSchema[n]", m.ConfigSchema["n"], "int"},
		{"命令数", len(m.Commands), 2},
		{"命令0.Prefix", m.Commands[0].Prefix, "/echo"},
		{"命令0.Loose", m.Commands[0].Loose, true},
		{"命令0.Priority", m.Commands[0].Priority, 5},
		{"命令0.Description", m.Commands[0].Description, "回显"},
		{"命令0.Permission", int(m.Commands[0].Permission), 4},
		{"命令0.DenyReply", m.Commands[0].DenyReply, "不行"},
		{"命令1.Pattern", m.Commands[1].Pattern, "^/re .*"},
		{"可选方法数", len(optional), 3},
		{"可选方法[2]", optional[2], "plugin.run_job"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %#v，期望 %#v", c.what, c.got, c.want)
		}
	}
}

// EncodeManifest 是探测侧（插件）与线协议共用的编码器：键名必须与
// wireManifest 逐字一致，且解回来不丢字段。
func TestEncodeManifestRoundTrip(t *testing.T) {
	var want struct {
		Manifest map[string]json.RawMessage
		Optional []string
	}
	if err := json.Unmarshal([]byte(wireManifest), &want); err != nil {
		t.Fatalf("字面量本身不合法: %v", err)
	}
	decoded, optional, err := DecodeManifest([]byte(wireManifest))
	if err != nil {
		t.Fatalf("解析字面量: %v", err)
	}

	got, err := EncodeManifest(decoded, optional)
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	var round struct {
		Manifest map[string]json.RawMessage
		Optional []string
	}
	if err := json.Unmarshal(got, &round); err != nil {
		t.Fatalf("编码结果不是合法 JSON: %v\n%s", err, got)
	}
	for key := range want.Manifest {
		if _, ok := round.Manifest[key]; !ok {
			t.Errorf("编码结果缺少 Manifest.%s（字段改名＝改协议）", key)
		}
	}
	if len(round.Optional) != len(want.Optional) {
		t.Fatalf("Optional = %v，期望 %v", round.Optional, want.Optional)
	}
}

// 插件把日志打到 stdout（违反了"stdout 是协议通道"）是现场最常见的形态：
// 探测必须报出一条能看懂的错误，而不是把半截 JSON 当清单。
func TestDecodeManifestRejectsGarbage(t *testing.T) {
	for _, body := range []string{
		`echo plugin v1.0\n{"Manifest":{}}`,
		`{"Manifest":{}} trailing`,
		``,
	} {
		if _, _, err := DecodeManifest([]byte(body)); err == nil {
			t.Errorf("应拒绝 %q", body)
		}
	}
}

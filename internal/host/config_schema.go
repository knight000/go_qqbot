package host

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ValidateConfig 在装配期（插件 Init 之前）按 Manifest.ConfigSchema 校验
// 插件的 [plugins.<name>] 配置段（评审 §4.1）。
//
// 语义是**声明即必填**：schema 里出现的键必须有值，缺一个就启动失败。
// 所以有默认值的键不要写进 schema——默认值本来就允许不写，写进去等于把它
// 变成必填。空值另算：`key = ""` 是「存在但为空」，对 string 合法，
// 与「缺失」可区分。
//
// 支持四种类型，与 Params 的读取方法一一对应：string / bool / int / duration。
// 类型清单与各自的校验只由 configKinds 这一张表描述。
// 声明其它类型名同样在装配期报错——那说明插件作者对契约的理解有偏差，
// 静默忽略会把它变成下一个「只在纸上成立」的字段。
//
// 错误一次列出该段所有问题（按键名排序，输出稳定），每条都带插件名、
// 段名、键名与期望类型：配置写错时使用者要能直接去改，而不是猜。
func (m Manifest) ValidateConfig(section map[string]any) error {
	problems := make([]string, 0, len(m.ConfigSchema))
	for _, key := range sortedSchemaKeys(m.ConfigSchema) {
		kindName := m.ConfigSchema[key]
		// 类型名先判：schema 写错时不该被"缺键"掩盖，否则修正方向会指错。
		kind, ok := lookupConfigKind(kindName)
		if !ok {
			problems = append(problems, fmt.Sprintf("键 %q 的类型 %q 不受支持（仅支持 %s）",
				key, kindName, supportedConfigKindList()))
			continue
		}
		if msg, ok := checkConfigValue(kind, key, section); !ok {
			problems = append(problems, msg)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("插件 %s 的配置段 [plugins.%s] 不合法：\n  - %s",
		m.Name, m.Name, strings.Join(problems, "\n  - "))
}

// configKind 是一种受支持的类型。
//
// 表里的顺序就是错误文案里类型清单的顺序（map 会打乱顺序，所以用切片），
// 加第五种类型只动这一处：名字、取值是否合法、以及"为什么不合法"都在同一行。
type configKind struct {
	name  string
	match func(v any) bool
	// reason 在 match 失败时给出更具体的原因；返回空串表示用默认的
	// 「期望 X，实际是 T(v)」。duration 用它区分"不是字符串"与"解析不了"。
	reason func(key string, v any) string
}

// configKinds 是类型表：名字、判定与取值校验的唯一来源。
var configKinds = []configKind{
	{name: "string", match: func(v any) bool {
		_, ok := v.(string)
		return ok
	}},
	{name: "bool", match: func(v any) bool {
		_, ok := v.(bool)
		return ok
	}},
	{name: "int", match: isConfigInt},
	{
		name: "duration",
		match: func(v any) bool {
			s, ok := v.(string)
			if !ok {
				return false
			}
			_, err := time.ParseDuration(s)
			return err == nil
		},
		reason: func(key string, v any) string {
			s, ok := v.(string)
			if !ok {
				return "" // 不是字符串：交给默认文案
			}
			return fmt.Sprintf("键 %q 期望 duration（能被 time.ParseDuration 解析，如 \"10s\"），实际是 string(%q)", key, s)
		},
	},
}

// lookupConfigKind 查类型表；ok=false 表示这个类型名不受支持。
func lookupConfigKind(name string) (configKind, bool) {
	for _, k := range configKinds {
		if k.name == name {
			return k, true
		}
	}
	return configKind{}, false
}

// supportedConfigKindList 把类型表拼成错误文案里的清单（顺序即表顺序）。
func supportedConfigKindList() string {
	names := make([]string, len(configKinds))
	for i, k := range configKinds {
		names[i] = k.name
	}
	return strings.Join(names, " / ")
}

// checkConfigValue 校验单个键，返回（错误描述, 是否通过）。
func checkConfigValue(kind configKind, key string, section map[string]any) (string, bool) {
	v, ok := section[key]
	if !ok || v == nil {
		return fmt.Sprintf("缺少键 %q（期望 %s；config_schema 声明即必填）", key, kind.name), false
	}
	if kind.match(v) {
		return "", true
	}
	if kind.reason != nil {
		if msg := kind.reason(key, v); msg != "" {
			return msg, false
		}
	}
	return typeMismatch(key, kind.name, v), false
}

// isConfigInt 认 int64 与 int：TOML 解出来的整数是 int64，
// int 只是防御性兼容（测试与直接构造 Params 时会用到）。
func isConfigInt(v any) bool {
	switch v.(type) {
	case int64, int:
		return true
	}
	return false
}

// typeMismatch 把「键、期望、实际」写在同一行：实际值也打印出来，
// 因为类型写错时最常见的下一步就是去配置里找那一行。
func typeMismatch(key, kind string, v any) string {
	return fmt.Sprintf("键 %q 期望 %s，实际是 %T(%v)", key, kind, v, v)
}

// sortedSchemaKeys 让校验顺序与错误输出与 map 迭代顺序无关。
func sortedSchemaKeys(schema map[string]string) []string {
	keys := make([]string, 0, len(schema))
	for k := range schema {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

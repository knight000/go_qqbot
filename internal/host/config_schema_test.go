package host

import (
	"strings"
	"testing"
)

// 声明即必填：schema 里写了 string 键而配置段里没有，启动期就要失败，
// 错误必须能一眼定位到「哪个插件、哪个段、哪个键、期望什么类型」。
func TestManifestValidateConfigMissingStringKeyLocatesPluginSectionKeyAndType(t *testing.T) {
	m := Manifest{
		Name:         "demo",
		ConfigSchema: map[string]string{"reply_prefix": "string"},
	}

	err := m.ValidateConfig(nil)
	if err == nil {
		t.Fatal("缺键应返回错误")
	}
	for _, want := range []string{"demo", "plugins.demo", "reply_prefix", "string"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误缺少 %q: %v", want, err)
		}
	}
}

// 类型不符同样是启动期错误，错误要指出实际拿到了什么类型。
func TestManifestValidateConfigRejectsStringTypeMismatch(t *testing.T) {
	m := Manifest{
		Name:         "demo",
		ConfigSchema: map[string]string{"reply_prefix": "string"},
	}

	err := m.ValidateConfig(map[string]any{"reply_prefix": int64(7)})
	if err == nil {
		t.Fatal("类型不符应返回错误")
	}
	for _, want := range []string{"demo", "plugins.demo", "reply_prefix", "string", "int64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误缺少 %q: %v", want, err)
		}
	}
}

// `key = ""` 是「存在但为空」，对 string 合法，不能当成缺失。
func TestManifestValidateConfigAcceptsEmptyString(t *testing.T) {
	m := Manifest{
		Name:         "demo",
		ConfigSchema: map[string]string{"reply_prefix": "string"},
	}

	if err := m.ValidateConfig(map[string]any{"reply_prefix": ""}); err != nil {
		t.Fatalf("空串应视为合法值: %v", err)
	}
}

// bool 与 int 各自的两条失败路径：缺失、类型不符。
// 类型判定是严格的——Params.Bool 容忍字符串 "true"，但 schema 是契约，
// 配置写成字符串就该在启动期改掉，而不是等读取时被悄悄接受。
func TestManifestValidateConfigCoversBoolAndInt(t *testing.T) {
	m := Manifest{
		Name: "demo",
		ConfigSchema: map[string]string{
			"enabled": "bool",
			"retries": "int",
		},
	}

	cases := []struct {
		name    string
		section map[string]any
		// want 为空表示期望通过；否则每条子串都必须出现在错误里。
		want []string
	}{
		{"合法", map[string]any{"enabled": true, "retries": int64(3)}, nil},
		{"int 也接受 int", map[string]any{"enabled": false, "retries": 2}, nil},
		{"bool 缺失", map[string]any{"retries": int64(3)}, []string{"enabled", "bool", "缺少"}},
		{"bool 类型不符", map[string]any{"enabled": "true", "retries": int64(3)}, []string{"enabled", "bool", "string"}},
		{"int 缺失", map[string]any{"enabled": true}, []string{"retries", "int", "缺少"}},
		{"int 位置写了字符串", map[string]any{"enabled": true, "retries": "many"}, []string{"retries", "int", "string"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := m.ValidateConfig(c.section)
			if len(c.want) == 0 {
				if err != nil {
					t.Fatalf("期望通过，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("错误缺少 %q: %v", w, err)
				}
			}
		})
	}
}

// duration 是字符串且必须能被 time.ParseDuration 解析；
// 解析失败算「类型不符」，错误里要带上实际写的那个值。
func TestManifestValidateConfigCoversDuration(t *testing.T) {
	m := Manifest{
		Name:         "bootnotice",
		ConfigSchema: map[string]string{"delay": "duration"},
	}

	if err := m.ValidateConfig(map[string]any{"delay": "10s"}); err != nil {
		t.Fatalf("可解析的 duration 应通过: %v", err)
	}

	t.Run("缺失", func(t *testing.T) {
		err := m.ValidateConfig(nil)
		if err == nil {
			t.Fatal("缺键应报错")
		}
		for _, w := range []string{"bootnotice", "delay", "duration", "缺少"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("错误缺少 %q: %v", w, err)
			}
		}
	})

	t.Run("写成数字", func(t *testing.T) {
		err := m.ValidateConfig(map[string]any{"delay": int64(10)})
		if err == nil {
			t.Fatal("数字不是 duration 的字面形式，应报错")
		}
		for _, w := range []string{"delay", "duration", "int64"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("错误缺少 %q: %v", w, err)
			}
		}
	})

	t.Run("解析失败", func(t *testing.T) {
		err := m.ValidateConfig(map[string]any{"delay": "10秒"})
		if err == nil {
			t.Fatal("无法解析的字符串应报错")
		}
		for _, w := range []string{"delay", "duration", "10秒"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("错误缺少 %q: %v", w, err)
			}
		}
	})
}

// 四种之外的类型名同样是装配期错误——它说明插件作者对契约的理解有偏差，
// 静默忽略等于让 schema 变成下一个只在纸上成立的字段。
func TestManifestValidateConfigRejectsUnknownType(t *testing.T) {
	m := Manifest{
		Name:         "demo",
		ConfigSchema: map[string]string{"ratio": "float"},
	}

	for name, section := range map[string]map[string]any{
		"键有值":   {"ratio": 0.5},
		"键缺失":   nil,
		"键有字符串": {"ratio": "0.5"},
	} {
		t.Run(name, func(t *testing.T) {
			err := m.ValidateConfig(section)
			if err == nil {
				t.Fatal("未知类型应报错")
			}
			for _, w := range []string{"demo", "ratio", "float", "不受支持"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("错误缺少 %q: %v", w, err)
				}
			}
		})
	}
}

// 未声明 config_schema 的插件行为完全不变：不校验、不报错。
func TestManifestValidateConfigWithoutSchemaIsNoop(t *testing.T) {
	m := Manifest{Name: "adminping"}

	if err := m.ValidateConfig(nil); err != nil {
		t.Fatalf("无 schema 时不应报错: %v", err)
	}
	if err := m.ValidateConfig(map[string]any{"whatever": int64(1)}); err != nil {
		t.Fatalf("无 schema 时多余键也不该报错: %v", err)
	}
}

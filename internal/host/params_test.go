package host

import (
	"strings"
	"testing"
)

func TestParamsDecodeArraysAndNestedTables(t *testing.T) {
	p := NewParams("demo", map[string]any{
		"endpoints": []any{"user", "faction"},
		"enabled":   true,
		"retry": map[string]any{
			"times": int64(3),
			"delay": "2s",
		},
	})

	var cfg struct {
		Endpoints []string `toml:"endpoints"`
		Enabled   bool     `toml:"enabled"`
		Retry     struct {
			Times int    `toml:"times"`
			Delay string `toml:"delay"`
		} `toml:"retry"`
	}
	if err := p.Decode(&cfg); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(cfg.Endpoints) != 2 || cfg.Endpoints[0] != "user" || cfg.Endpoints[1] != "faction" {
		t.Errorf("数组解错: %v", cfg.Endpoints)
	}
	if !cfg.Enabled {
		t.Error("布尔值解错")
	}
	if cfg.Retry.Times != 3 || cfg.Retry.Delay != "2s" {
		t.Errorf("嵌套表解错: %+v", cfg.Retry)
	}
}

func TestParamsDecodeTypeMismatchNamesSectionAndKey(t *testing.T) {
	p := NewParams("torn", map[string]any{
		"retry": map[string]any{"times": "many"},
	})

	var cfg struct {
		Retry struct {
			Times int `toml:"times"`
		} `toml:"retry"`
	}
	err := p.Decode(&cfg)
	if err == nil {
		t.Fatal("类型不匹配应返回错误")
	}
	for _, want := range []string{"torn", "times"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应同时包含段名与键名，缺少 %q: %v", want, err)
		}
	}
}

func TestParamsDecodeEmptySectionIsNotAnError(t *testing.T) {
	var cfg struct {
		Limit int `toml:"limit"`
	}
	if err := NewParams("empty", nil).Decode(&cfg); err != nil {
		t.Fatalf("空配置段应返回零值而不是错误: %v", err)
	}
	if cfg.Limit != 0 {
		t.Errorf("空配置段应保持零值，实际 %d", cfg.Limit)
	}
}

func TestParamsDecodeRequiresPointer(t *testing.T) {
	var cfg struct {
		Limit int `toml:"limit"`
	}
	if err := NewParams("bad", map[string]any{"limit": int64(1)}).Decode(cfg); err == nil {
		t.Fatal("传入非指针应返回错误")
	}
	if err := NewParams("bad", map[string]any{"limit": int64(1)}).Decode(nil); err == nil {
		t.Fatal("传入 nil 应返回错误")
	}
}

// 空串是「存在但为空」，不是「没配」。声明为必填的字符串键被配成空串时，
// 插件必须拿到空串——否则 api_base 想置空却打到了默认的生产地址。
func TestParamsStrDistinguishesEmptyFromMissing(t *testing.T) {
	p := NewParams("demo", map[string]any{"empty": "", "number": int64(1)})

	if got := p.Str("empty", "def"); got != "" {
		t.Errorf("显式空串应原样返回，实际 %q", got)
	}
	if got := p.Str("missing", "def"); got != "def" {
		t.Errorf("缺失键应返回默认值，实际 %q", got)
	}
	if got := p.Str("number", "def"); got != "def" {
		t.Errorf("非字符串应返回默认值，实际 %q", got)
	}
}

func TestParamsAccessorsKeepDefaults(t *testing.T) {
	p := NewParams("demo", map[string]any{"limit": int64(5), "name": "x"})
	if got := p.Int("limit", 1); got != 5 {
		t.Errorf("Int=%d", got)
	}
	if got := p.Str("missing", "def"); got != "def" {
		t.Errorf("Str 默认值=%q", got)
	}
	if got := p.Int("missing", 9); got != 9 {
		t.Errorf("Int 默认值=%d", got)
	}
}

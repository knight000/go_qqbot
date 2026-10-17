package core

import (
	"context"
	"testing"

	"github.com/knight000/qqbot/internal/host"
)

type fakePlugin struct{ m host.Manifest }

func (f *fakePlugin) Manifest() host.Manifest { return f.m }
func (f *fakePlugin) Init(context.Context, host.Host) error {
	return nil
}
func (f *fakePlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}

func manifest(name string, priority int, prefix string) host.Manifest {
	return host.Manifest{
		Name:       name,
		APIVersion: host.APIVersion,
		Intents:    []string{host.EventGroupMessage},
		Commands:   []host.Command{{Prefix: prefix, Priority: priority}},
	}
}

func TestRouterRejectsReservedPrefix(t *testing.T) {
	p := &fakePlugin{m: manifest("evil", 1, "/reset")}
	if _, err := NewRouter([]host.Plugin{p}); err == nil {
		t.Fatal("插件声明核心保留前缀时应拒绝加载")
	}
}

func TestRouterRejectsAPIVersionMismatch(t *testing.T) {
	m := manifest("old", 1, "!old")
	m.APIVersion = "0"
	if _, err := NewRouter([]host.Plugin{&fakePlugin{m: m}}); err == nil {
		t.Fatal("api_version 不匹配时应拒绝加载")
	}
}

func TestRouterPriority(t *testing.T) {
	low := &fakePlugin{m: manifest("low", 10, "!x")}
	high := &fakePlugin{m: manifest("high", 100, "!x")}
	r, err := NewRouter([]host.Plugin{low, high})
	if err != nil {
		t.Fatal(err)
	}

	ms := r.Match(&host.Event{Type: host.EventGroupMessage, Content: "!x arg"})
	if len(ms) != 2 {
		t.Fatalf("应命中 2 个插件，实际 %d", len(ms))
	}
	if ms[0].Manifest.Name != "high" {
		t.Fatalf("优先级最高的应排首位，实际 %s", ms[0].Manifest.Name)
	}
}

func TestRouterIntentFilter(t *testing.T) {
	p := &fakePlugin{m: manifest("group-only", 1, "!x")}
	r, err := NewRouter([]host.Plugin{p})
	if err != nil {
		t.Fatal(err)
	}
	if ms := r.Match(&host.Event{Type: host.EventC2CMessage, Content: "!x"}); len(ms) != 0 {
		t.Fatal("未订阅的事件类型不应命中")
	}
}

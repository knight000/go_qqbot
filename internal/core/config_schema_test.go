package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// schemaPlugin 记录 Init 有没有被调用：配置校验必须发生在 Init 之前，
// 否则插件已经拿零值跑起来了，报错也就晚了。
type schemaPlugin struct {
	manifest host.Manifest
	inited   bool
}

func (p *schemaPlugin) Manifest() host.Manifest { return p.manifest }

func (p *schemaPlugin) Init(context.Context, host.Host) error {
	p.inited = true
	return nil
}

func (p *schemaPlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}

func newSchemaCore(t *testing.T, cfg *config.Config, plugins ...host.Plugin) *Core {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	c, err := New(Options{
		Store:   db,
		Log:     slog.New(slog.DiscardHandler),
		Config:  cfg,
		Plugins: plugins,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// 声明了 config_schema 却缺键 → 装配期失败，错误定位到插件、段、键与期望类型，
// 且任何一个插件的 Init 都还没跑。
func TestStartFailsBeforeInitWhenConfigKeyMissing(t *testing.T) {
	ok := &schemaPlugin{manifest: host.Manifest{Name: "aaa", APIVersion: host.APIVersion}}
	bad := &schemaPlugin{manifest: host.Manifest{
		Name:         "demo",
		APIVersion:   host.APIVersion,
		ConfigSchema: map[string]string{"delay": "duration"},
	}}
	cfg := &config.Config{Plugins: config.Plugins{Sections: map[string]map[string]any{"demo": {"delay": "10s"}}}}
	cfg.Plugins.Sections["demo"] = map[string]any{} // 段存在但缺键

	c := newSchemaCore(t, cfg, ok, bad)

	err := c.Start(context.Background())
	if err == nil {
		t.Fatal("缺键应导致启动失败")
	}
	for _, want := range []string{"demo", "plugins.demo", "delay", "duration"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误缺少 %q: %v", want, err)
		}
	}
	if bad.inited {
		t.Error("校验失败时不应调用 Init")
	}
	if ok.inited {
		t.Error("有插件校验失败时，其它插件也不该已经开始 Init")
	}
}

// 未声明 config_schema 的插件行为完全不变：空配置段照样能启动。
func TestStartKeepsWorkingWithoutConfigSchema(t *testing.T) {
	p := &schemaPlugin{manifest: host.Manifest{Name: "adminping", APIVersion: host.APIVersion}}

	c := newSchemaCore(t, &config.Config{}, p)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := c.Start(ctx); err != nil {
		t.Fatalf("无 schema 的插件应照常启动: %v", err)
	}
	if !p.inited {
		t.Fatal("Init 应被调用")
	}
}

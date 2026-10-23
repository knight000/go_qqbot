package core

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/internal/store"
	"github.com/knight000/go_qqbot/pkg/host"
)

// newIntentCore 走真实的 Start：交叉校验挂在装配流程里，直接调内部函数
// 测不到"Start 到底有没有接上"。
func newIntentCore(t *testing.T, bits uint64, intents ...string) (*Core, *bytes.Buffer) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "intents.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	p := &schemaPlugin{manifest: host.Manifest{
		Name:       "gap",
		APIVersion: host.APIVersion,
		Intents:    intents,
	}}
	c, err := New(Options{
		Store:   db,
		Log:     logger,
		Config:  &config.Config{Bot: config.Bot{Intents: bits}},
		Plugins: []host.Plugin{p},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, &buf
}

func startWithIntentCore(t *testing.T, c *Core) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// 插件声明 notice、配置却只开了 INTERACTION：路由表里有插件，网关永远收不到通知，
// 启动日志一切正常——这是最难排查的一类"静默不工作"，必须在装配期留一条 Warn。
func TestStartWarnsWhenPluginIntentsAreNotEnabled(t *testing.T) {
	c, buf := newIntentCore(t, 1<<26, "notice")
	startWithIntentCore(t, c)

	out := buf.String()
	for _, want := range []string{"level=WARN", "plugin=gap", "intent=notice", "1<<25"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志应包含 %q，实际:\n%s", want, out)
		}
	}
}

func TestStartQuietWhenIntentsMatch(t *testing.T) {
	c, buf := newIntentCore(t, 1<<25, "message", "group.notice")
	startWithIntentCore(t, c)

	if strings.Contains(buf.String(), "missing_bit") {
		t.Errorf("intents 已覆盖时不该告警:\n%s", buf.String())
	}
}

func TestStartChecksMetaAgainstInteractionBit(t *testing.T) {
	c, buf := newIntentCore(t, 1<<25, "meta")
	startWithIntentCore(t, c)

	out := buf.String()
	if !strings.Contains(out, "missing_bit") || !strings.Contains(out, "1<<26") {
		t.Errorf("meta 缺 INTERACTION 位应告警:\n%s", out)
	}
}

package rpc

// 本文件测「使用面在清单出口合并」这条路（#56）。
//
// 为什么值得单独测：生成器产出的成员、作者手写的追加项、插件的 Manifest()
// 是三个来源，合错一个方向都是灾难——漏并等于插件被判成「没声明使用面」
// （退化为整体相等，机制白装），并错等于凭空多出几条它没用到的成员
// （把不该拒的插件拒掉）。

import (
	"context"
	"slices"
	"testing"

	"github.com/knight000/go_qqbot/pkg/host"
)

// surfacePlugin 是带 APISurface 的插件替身——生成器写出来的就是这个方法。
type surfacePlugin struct {
	m       host.Manifest
	surface []string
}

func (p *surfacePlugin) Manifest() host.Manifest { return p.m }
func (p *surfacePlugin) APISurface() []string    { return p.surface }
func (p *surfacePlugin) Init(context.Context, host.Host) error {
	return nil
}
func (p *surfacePlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}

// plainPlugin 没有 APISurface：等价于「忘了跑生成器」。
type plainPlugin struct{ m host.Manifest }

func (p *plainPlugin) Manifest() host.Manifest               { return p.m }
func (p *plainPlugin) Init(context.Context, host.Host) error { return nil }
func (p *plainPlugin) Handle(context.Context, *host.Event) (*host.Reply, error) {
	return nil, nil
}

func TestPluginManifestMergesSurface(t *testing.T) {
	cases := []struct {
		name      string
		manual    []string
		generated []string
		want      []string
	}{
		{
			name:      "生成项并入，重复项去掉",
			manual:    []string{"Event.Args"},
			generated: []string{"Event.Args", "Params.Duration", "JobSpec"},
			want:      []string{"Event.Args", "JobSpec", "Params.Duration"},
		},
		{
			name:      "只有生成项时按字典序输出（顺序稳定才可比对 diff）",
			generated: []string{"Host.Renderer", "Event.Args", "Manifest"},
			want:      []string{"Event.Args", "Host.Renderer", "Manifest"},
		},
		{
			name:      "手写项的空格与空串被清掉（否则字符串求交永远不相交）",
			manual:    []string{" Event.Args ", "", "  "},
			generated: []string{"JobSpec"},
			want:      []string{"Event.Args", "JobSpec"},
		},
		{
			name:   "生成结果为空：手写项原样返回（不排序，保持可辨认）",
			manual: []string{"Params.Str", "Event.Args"},
			want:   []string{"Params.Str", "Event.Args"},
		},
		{
			name:   "生成结果为空：手写项仍要做规范化（空格/空串清掉，顺序不动）",
			manual: []string{" Event.Args ", "", "  "},
			want:   []string{"Event.Args"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &surfacePlugin{m: host.Manifest{Name: "x", Uses: c.manual}, surface: c.generated}
			if got := PluginManifest(p).Uses; !slices.Equal(got, c.want) {
				t.Fatalf("Uses = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestPluginManifestWithoutSurface 锁住「未生成」的方向：Uses 保持作者写的
// 那一份（通常是空），兼容判定据此退化为「api_version 必须与核心相等」。
//
// 方向不能反：若把「没有生成」当成「不依赖任何成员」，生成器一挂就等于所有
// 插件放行，语义变更静默穿透（ADR-0007 决策 5）。
func TestPluginManifestWithoutSurface(t *testing.T) {
	p := &plainPlugin{m: host.Manifest{Name: "x"}}
	if got := PluginManifest(p).Uses; got != nil {
		t.Fatalf("Uses = %v，期望 nil（未生成时必须留空，交给 CheckCompat 退化为整体相等）", got)
	}
}

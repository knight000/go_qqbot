package instance

// 渲染是进程里最吃内存的那件事：全局并发 2 是按实测调的（1080×4000 的 RGBA
// 约 17.3 MB，16 个 worker 峰值 277 MB 会被内核杀掉）。全局上限不动，但如果
// 一个租户能一口气吃掉那 2 格，另一个租户的渲染就只能排队——这就是资源池的
// 跨租户污染。用例因此只在「两个 Bot 同时活着」时才成立。

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/internal/config"
	"github.com/knight000/go_qqbot/pkg/host"
)

// blockingRenderer 是注入给容器的共享渲染器：每次调用都停在这里，直到测试
// 放开。用它把"同时在画几张"变成可断言的事实，而不是靠 sleep 猜。
type blockingRenderer struct {
	release chan struct{}
	entered chan string

	mu       sync.Mutex
	inflight map[string]int
	peak     map[string]int
	total    int
}

func newBlockingRenderer() *blockingRenderer {
	return &blockingRenderer{
		release:  make(chan struct{}),
		entered:  make(chan string, 16),
		inflight: map[string]int{},
		peak:     map[string]int{},
	}
}

func (b *blockingRenderer) Markdown(ctx context.Context, src string) (host.Image, error) {
	b.mu.Lock()
	b.inflight[src]++
	b.total++
	if b.inflight[src] > b.peak[src] {
		b.peak[src] = b.inflight[src]
	}
	b.mu.Unlock()
	select {
	case b.entered <- src:
	default:
	}

	select {
	case <-b.release:
	case <-ctx.Done():
		b.leave(src)
		return host.Image{}, ctx.Err()
	}
	b.leave(src)
	return host.Image{}, nil
}

func (b *blockingRenderer) leave(src string) {
	b.mu.Lock()
	b.inflight[src]--
	b.mu.Unlock()
}

// open 放开所有仍在排队的渲染。
func (b *blockingRenderer) open() { close(b.release) }

// snapshot 返回当前的每源在飞数、历史峰值与总调用数。
func (b *blockingRenderer) snapshot() (inflight, peak map[string]int, total int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	inflight = make(map[string]int, len(b.inflight))
	for k, v := range b.inflight {
		inflight[k] = v
	}
	peak = make(map[string]int, len(b.peak))
	for k, v := range b.peak {
		peak[k] = v
	}
	return inflight, peak, b.total
}

// renderPlugin 在处理每条事件时同步发起一次渲染，源就是消息正文：
// 这样"哪张图是哪个 Bot 要的"在渲染器一侧可读。
type renderPlugin struct {
	name string

	mu sync.Mutex
	h  host.Host
}

func (p *renderPlugin) Manifest() host.Manifest {
	return host.Manifest{
		Name:       p.name,
		Version:    "0.0.1",
		APIVersion: host.APIVersion,
		Intents:    []string{"message"},
	}
}

func (p *renderPlugin) Init(_ context.Context, h host.Host) error {
	p.mu.Lock()
	p.h = h
	p.mu.Unlock()
	return nil
}

func (p *renderPlugin) Handle(ctx context.Context, ev *host.Event) (*host.Reply, error) {
	p.mu.Lock()
	h := p.h
	p.mu.Unlock()
	if _, err := h.Renderer().Markdown(ctx, ev.Content); err != nil {
		return nil, err
	}
	return &host.Reply{Content: "rendered:" + ev.Content}, nil
}

// 每 Bot 同时最多 1 个渲染任务；不同 Bot 之间仍可同时画。
//
// 断言取的是"当时在飞的两张图分别属于谁"：一个 Bot 连推三条时，共享渲染器
// 上最多只该有它一张；此时另一个 Bot 的那张必须已经在画——否则说明单租户
// 仍然能占满全局池，另一个租户只能排队。
func TestRenderIsSerializedPerBot(t *testing.T) {
	p := newFakePlatform(t)
	cfg := testConfig(t, p, "102000001", "102000002")
	block := newBlockingRenderer()
	startContainer(t, cfg, p, func(o *Options) {
		o.Renderer = block
		o.LoadPlugins = func(context.Context, config.Bot, *slog.Logger) ([]host.Plugin, error) {
			return []host.Plugin{&renderPlugin{name: "md"}}, nil
		}
	})

	p.waitReady(t, "102000001")
	p.waitReady(t, "102000002")

	// A 一口气三条，B 一条：这是"单租户连续提交"的形态。
	//
	// 三条必须落在三个不同的群上：分发层按 (场景, peer) 串行，同一个群的
	// 三条消息本来就不会同时处理——那样测出来的是分发层的串行，不是闸门。
	for _, c := range []string{"a1", "a2", "a3"} {
		p.push(t, "102000001", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-"+c, "G-"+c, c))
	}
	p.push(t, "102000002", "GROUP_AT_MESSAGE_CREATE", groupMessage("EV-b1", "G-b1", "b1"))

	// 等到 A、B 各有一张在画。
	waitInFlight(t, block, 2)

	inflight, _, _ := block.snapshot()
	perBot := map[byte]int{}
	for src, n := range inflight {
		perBot[src[0]] += n
	}
	if perBot['a'] != 1 {
		t.Fatalf("A 同时在画 %d 张，每 Bot 上限是 1（在飞: %v）", perBot['a'], inflight)
	}
	if perBot['b'] != 1 {
		t.Fatalf("B 应当在画自己的一张（在飞: %v）", inflight)
	}

	block.open()

	// 放开之后 A 剩下两张必须逐张画完，且没有任何一刻同源并发 > 1。
	waitRendered(t, block, 4)
	_, peak, total := block.snapshot()
	if total != 4 {
		t.Fatalf("四个事件应各渲染一次，实际 %d 次", total)
	}
	for src, n := range peak {
		if n != 1 {
			t.Fatalf("源 %q 的并发峰值为 %d，应为 1（每 Bot 一张）", src, n)
		}
	}
}

// waitInFlight 等渲染器里同时在画的数量达到 n（超时算失败）。
func waitInFlight(t *testing.T, b *blockingRenderer, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		inflight, _, _ := b.snapshot()
		sum := 0
		for _, v := range inflight {
			sum += v
		}
		if sum >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	inflight, _, _ := b.snapshot()
	t.Fatalf("等不到 %d 个渲染同时在画，实际在飞: %v", n, inflight)
}

// waitRendered 等渲染调用总数达到 n。
func waitRendered(t *testing.T, b *blockingRenderer, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, total := b.snapshot(); total >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, total := b.snapshot()
	t.Fatalf("等不到 %d 次渲染完成，实际 %d 次", n, total)
}

// 渲染器的错误必须原样透出给插件，闸门不能把它变成"成功"或换一个错误。
func TestRenderGateKeepsInnerError(t *testing.T) {
	boom := errRenderBoom{}
	g := newBotRenderer(boom)
	if _, err := g.Markdown(context.Background(), "x"); err != boom {
		t.Fatalf("内层错误应原样返回，实际 %v", err)
	}
}

type errRenderBoom struct{}

func (errRenderBoom) Error() string { return "boom" }
func (errRenderBoom) Markdown(context.Context, string) (host.Image, error) {
	return host.Image{}, errRenderBoom{}
}

// 闸门排队的顺序不做保证，但每个拿到槽位的调用都必须真的进到内层：
// 不能因为等槽位就静默跳过（那会变成"机器人不出图"）。
func TestRenderGateRunsEveryCall(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	inner := renderFunc(func(_ context.Context, src string) (host.Image, error) {
		mu.Lock()
		seen = append(seen, src)
		mu.Unlock()
		return host.Image{}, nil
	})
	g := newBotRenderer(inner)

	var wg sync.WaitGroup
	for _, src := range []string{"1", "2", "3", "4"} {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			if _, err := g.Markdown(context.Background(), src); err != nil {
				t.Errorf("渲染 %s 失败: %v", src, err)
			}
		}(src)
	}
	wg.Wait()

	if len(seen) != 4 {
		t.Fatalf("四次调用都该进入内层渲染器，实际 %v", seen)
	}
}

// renderFunc 把函数变成 host.Renderer，省掉一个只有一行的类型。
type renderFunc func(context.Context, string) (host.Image, error)

func (f renderFunc) Markdown(ctx context.Context, src string) (host.Image, error) { return f(ctx, src) }

// 未注入渲染器时必须保持 nil 语义：host 会换成"拒绝一切"的替身，插件据此
// 退回纯文本；包一层非 nil 的空壳会让它变成一个 nil 解引用。
func TestRenderGateStaysNilWhenNotInjected(t *testing.T) {
	if got := renderGateFor(nil); got != nil {
		t.Fatalf("没有注入渲染器时不该造出空壳，实际 %T", got)
	}
	if got := renderGateFor(renderFunc(func(context.Context, string) (host.Image, error) {
		return host.Image{}, nil
	})); got == nil {
		t.Fatal("注入了渲染器就必须包上每 Bot 的闸门")
	}
}

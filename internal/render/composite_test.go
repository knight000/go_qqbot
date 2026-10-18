package render

import "testing"

// compositeSample 把全部受支持的块放在一份文档里，用来检查块与块的相互作用：
// 表格、嵌套列表、引用里的列表、代码块紧挨分隔线。
const compositeSample = "# 构建汇总 · 2026-10-18\n" +
	"\n" +
	"本次构建覆盖 **3 个平台**，其中 `linux/amd64` 与 `windows/amd64` 通过，\n" +
	"`linux/arm64` 因交叉编译链缺失而~~跳过~~，详细产物见 [发布页](https://example.com/releases)。\n" +
	"\n" +
	"## 服务状态\n" +
	"\n" +
	"| 服务 | 版本 | 状态 | 最近检查 |\n" +
	"| --- | --- | --- | --- |\n" +
	"| qqbot | 0.9.4 | 运行中 | 20:41 |\n" +
	"| frps | 0.71.0 | 运行中 | 20:40 |\n" +
	"| aria2 | 1.37.0 | 已停止 | 19:12 |\n" +
	"| webdav | — | 降级 | 18:55 |\n" +
	"\n" +
	"### 备注\n" +
	"\n" +
	"- 磁盘剩余 **12.4 GB**，低于告警线 `20 GB`\n" +
	"- 昨日新增两处配置漂移，已修复一处\n" +
	"  - `/etc/qqbot/config.toml` 的 `[http].timeout` 被手工改过\n" +
	"  - 回滚只需把软链指回去\n" +
	"- 备份任务下一次执行在明日 03:00\n" +
	"\n" +
	"> 所有时间均为 Asia/Shanghai。重启后首次 `/ping` 可能包含启动耗时。\n" +
	">\n" +
	"> 1. 维护窗口：周日 03:00–04:00\n" +
	"> 2. 超过 30 秒未就绪则自动回滚\n" +
	"\n" +
	"- [x] 备份配置\n" +
	"- [ ] 更新证书\n" +
	"\n" +
	"```bash\n" +
	"go build -trimpath -ldflags \"-s -w\" -o bin/qqbot ./cmd/bot\n" +
	"GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags \"-s -w\" -tags netgo -o bin/qqbot-linux-amd64 ./cmd/bot\n" +
	"```\n" +
	"\n" +
	"---\n" +
	"\n" +
	"审核人：knight　｜　签发：自动流水线\n"

// 所有块元素放在同一篇里也要能出图，且高度不越上限。
func TestCompositeDocumentRenders(t *testing.T) {
	data := renderPNG(t, compositeSample)
	m := decodePNG(t, data)
	if m.Bounds().Dx() != int(imgWidth) {
		t.Errorf("宽 = %d，期望 %v", m.Bounds().Dx(), imgWidth)
	}
	if h := m.Bounds().Dy(); h <= 0 || h > maxHeight {
		t.Fatalf("高 = %d，期望 (0, %d]", h, maxHeight)
	}
	writeSample(t, "all.png", data)
}

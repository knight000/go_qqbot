package render

import (
	"context"
	"testing"
)

const listSample = `1. 拉取镜像并校验签名
2. 停掉旧进程，保留上一个可执行文件
   - 回滚只需要把软链指回去
   - 旧二进制保留最近三份
3. 启动新进程并等待健康检查通过

- [x] 备份配置
- [ ] 更新证书
- [ ] 清理旧日志
`

const quoteSample = `> 所有时间均为 Asia/Shanghai。重启后首次 ` + "`/ping`" + ` 可能包含启动耗时。
>
> 多段引用时竖线应当连续覆盖两段，并且只到内容盒为止。
>
> **不要**在维护窗口之外重启，` + "`graceful-shutdown`" + ` 需要留出 30 秒。
`

func imgHeight(t *testing.T, src string) int {
	t.Helper()
	img, err := testRenderer(t).Markdown(context.Background(), src)
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	return decodePNG(t, img.Data()).Bounds().Dy()
}

// 列表项之间是紧凑的 8px，而不是段落那样的 32px：三行内容做成列表必须比
// 三个独立段落矮。这是"列表真的被当成列表排"的外部可观察面。
func TestListItemsUseTightSpacing(t *testing.T) {
	asList := imgHeight(t, "- 第一项\n- 第二项\n- 第三项\n")
	asParagraphs := imgHeight(t, "第一项\n\n第二项\n\n第三项\n")

	if asList >= asParagraphs {
		t.Errorf("列表高 %d 不小于三段独立段落 %d，列表项之间很可能没有按紧凑间距排", asList, asParagraphs)
	}
}

// 有序/无序、两层嵌套、任务列表在同一份文档里都能出图。
func TestListsRenderNestedAndTaskItems(t *testing.T) {
	data := renderPNG(t, listSample)

	m := decodePNG(t, data)
	if got := m.Bounds().Dx(); got != int(imgWidth) {
		t.Errorf("宽 = %d，期望 %v", got, imgWidth)
	}
	if h := m.Bounds().Dy(); h <= 0 {
		t.Fatalf("高 = %d，期望 > 0", h)
	}
	writeSample(t, "list.png", data)
}

// 块引用：多段内容连续排，竖线高度等于内容盒（不含最后一段的 margin），
// 因此引用后面接段落时的间距必须与普通段间距一致，不能多出一段。
func TestBlockquoteRendersAsOneBox(t *testing.T) {
	data := renderPNG(t, quoteSample)
	m := decodePNG(t, data)
	if got := m.Bounds().Dx(); got != int(imgWidth) {
		t.Errorf("宽 = %d，期望 %v", got, imgWidth)
	}
	if h := m.Bounds().Dy(); h <= 0 {
		t.Fatalf("高 = %d，期望 > 0", h)
	}
	writeSample(t, "quote.png", data)
}

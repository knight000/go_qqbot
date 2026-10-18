package render

import (
	"strings"
	"testing"
)

const codeSample = "```bash\n" +
	"go build -trimpath -ldflags \"-s -w\" -o bin/qqbot ./cmd/bot\n" +
	"GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags \"-s -w\" -tags netgo -o bin/qqbot-linux-amd64 ./cmd/bot\n" +
	"# 保留最近三份旧二进制，回滚只需要改软链\n" +
	"\n" +
	"go test ./... -count=1\n" +
	"```\n"

// 超宽代码行必须折行：内容变长时块高按行增加，而不是被截断后高度不变。
func TestCodeBlockWrapsLongLines(t *testing.T) {
	short := "```\nfmt.Println(1)\n```\n"
	long := "```\n" + strings.Repeat("go-build-", 60) + "\n```\n"

	hs, hl := imgHeight(t, short), imgHeight(t, long)
	if hl <= hs {
		t.Errorf("超宽代码块高 %d 不大于单行 %d，长行很可能被截断或直接溢出画布", hl, hs)
	}
}

// 多行、含中文注释与空行的代码块能正常出图。
func TestCodeBlockRendersWithBackground(t *testing.T) {
	data := renderPNG(t, codeSample)
	m := decodePNG(t, data)
	if got := m.Bounds().Dx(); got != int(imgWidth) {
		t.Errorf("宽 = %d，期望 %v", got, imgWidth)
	}
	if h := m.Bounds().Dy(); h <= 0 {
		t.Fatalf("高 = %d，期望 > 0", h)
	}
	writeSample(t, "code.png", data)
}

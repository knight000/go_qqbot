package core

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/internal/host"
)

// pngHeader 是 PNG 的 8 字节签名，够 http.DetectContentType 认出来。
var pngHeader = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

func tinyPNG() []byte {
	b := make([]byte, 64)
	copy(b, pngHeader)
	return b
}

func imageSender(t *testing.T) (*Sender, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{}
	return NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{}, nil), f
}

func msgEvent() *host.Event {
	return &host.Event{Scene: host.SceneGroup, PeerID: "g1", MsgID: "m1"}
}

// 平台靠 file_name 的扩展名判格式：已知的沿用、别名收敛、未知的按内容嗅探。
func TestImageNameNormalization(t *testing.T) {
	cases := []struct {
		name    string
		data    []byte
		want    string
		wantErr bool
	}{
		{name: "chart.png", want: "chart.png"},
		{name: "photo.JPEG", want: "photo.jpg"},
		{name: "photo", data: tinyPNG(), want: "image.png"},
		{name: "photo.txt", data: tinyPNG(), want: "image.png"},
		{name: "photo.txt", data: []byte("这不是图片"), wantErr: true},
	}
	for _, c := range cases {
		got, err := imageName(c.name, c.data)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q 应报无法识别，实际得到 %q", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 不应报错: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q 应规整成 %q，实际 %q", c.name, c.want, got)
		}
	}
}

// 图片上要先换 file_info，再用 msg_type=7 发出去——两步的顺序是平台规定的。
func TestReplyImageUploadsThenSendsMedia(t *testing.T) {
	s, f := imageSender(t)
	ctx := context.Background()

	if err := s.Reply(ctx, msgEvent(), host.Reply{Image: host.ImageFromBytes(tinyPNG())}); err != nil {
		t.Fatalf("发图不应失败: %v", err)
	}
	if n := f.uploadCount(); n != 1 {
		t.Fatalf("应上传 1 次，实际 %d", n)
	}
	up := f.uploadAt(0)
	if up.FileType != api.MediaImage {
		t.Errorf("file_type 应为图片(%d)，实际 %d", api.MediaImage, up.FileType)
	}
	if up.FileData == "" || up.URL != "" {
		t.Errorf("内存字节应走 file_data 而非 url: %+v", up)
	}
	if up.FileName != "image.png" {
		t.Errorf("文件名应由内容嗅探成 image.png，实际 %q", up.FileName)
	}
	if up.SrvSendMsg {
		t.Error("srv_send_msg 必须为 false：发送要经过核心的频控与 msg_seq")
	}

	if n := f.callCount(); n != 1 {
		t.Fatalf("应发 1 条消息，实际 %d", n)
	}
	body := f.bodyAt(0)
	if body.MsgType != 7 {
		t.Errorf("msg_type 应为 7，实际 %d", body.MsgType)
	}
	if body.Content != "" {
		t.Errorf("图片消息不应带正文，实际 %q", body.Content)
	}
	if body.Media == nil || body.Media.FileInfo != "file-info-1" {
		t.Errorf("media.file_info 应透传上传结果，实际 %+v", body.Media)
	}
	if body.MsgID != "m1" || body.MsgSeq != 1 {
		t.Errorf("被动回复应带 msg_id 与自增 seq，实际 %q/%d", body.MsgID, body.MsgSeq)
	}
}

// 图文要拆成两条：平台一条消息只能有一种 msg_type。顺序固定为文本在前。
func TestReplyTextAndImageSplitsIntoTwoMessages(t *testing.T) {
	s, f := imageSender(t)
	ctx := context.Background()

	err := s.Reply(ctx, msgEvent(), host.Reply{
		Content: "查询结果：",
		Image:   host.ImageFromBytes(tinyPNG()),
	})
	if err != nil {
		t.Fatalf("图文回复不应失败: %v", err)
	}
	if n := f.callCount(); n != 2 {
		t.Fatalf("应发两条消息，实际 %d", n)
	}
	first, second := f.bodyAt(0), f.bodyAt(1)
	if first.Content != "查询结果：" || first.MsgType != 0 || first.Media != nil {
		t.Errorf("第一条应是纯文本，实际 %+v", first)
	}
	if second.MsgType != 7 || second.Media == nil {
		t.Errorf("第二条应是图片，实际 %+v", second)
	}
	if first.MsgSeq != 1 || second.MsgSeq != 2 {
		t.Errorf("seq 应依次递增，实际 %d/%d", first.MsgSeq, second.MsgSeq)
	}
	if n := f.uploadCount(); n != 1 {
		t.Errorf("两条消息共用一次上传，实际上传 %d 次", n)
	}
}

// URL 来源不读本地、不上传字节，只把地址交给平台。
func TestReplyImageFromURLUsesPlatformFetch(t *testing.T) {
	s, f := imageSender(t)
	ctx := context.Background()

	if err := s.Reply(ctx, msgEvent(), host.Reply{
		Image: host.ImageFromURL("https://example.com/a.png"),
	}); err != nil {
		t.Fatalf("URL 发图不应失败: %v", err)
	}
	up := f.uploadAt(0)
	if up.URL != "https://example.com/a.png" {
		t.Errorf("应透传图片 URL，实际 %q", up.URL)
	}
	if up.FileData != "" {
		t.Error("URL 来源不应带 file_data")
	}
}

// 超过 5 MB 的图必须走分片上传，而分片没实现——本地就拒掉，
// 别把 20 MB 的图 base64 成 27 MB 再被平台打回。
func TestReplyImageTooLargeRejectedLocally(t *testing.T) {
	s, f := imageSender(t)
	data := make([]byte, api.MaxInlineMediaBytes+1)
	copy(data, pngHeader)

	err := s.Reply(context.Background(), msgEvent(), host.Reply{Image: host.ImageFromBytes(data)})
	if err == nil || !strings.Contains(err.Error(), "直传上限") {
		t.Fatalf("应提示超过直传上限，实际 err=%v", err)
	}
	if n := f.uploadCount(); n != 0 {
		t.Errorf("超限时不该上传，实际 %d 次", n)
	}
	if n := f.callCount(); n != 0 {
		t.Errorf("超限时不该发消息，实际 %d 次", n)
	}
}

// 上传失败不该吃掉被动回复配额：否则一次图裂，用户在 5 分钟内就只能
// 收到四次回复。
func TestReplyUploadFailureKeepsPassiveWindow(t *testing.T) {
	s, f := imageSender(t)
	ctx := context.Background()
	ev := msgEvent()

	f.uploadErr = errors.New("模拟上传失败")
	if err := s.Reply(ctx, ev, host.Reply{Image: host.ImageFromBytes(tinyPNG())}); err == nil {
		t.Fatal("上传失败时应返回错误")
	}
	if n := f.callCount(); n != 0 {
		t.Fatalf("上传失败时不该发消息，实际 %d 次", n)
	}

	f.uploadErr = nil
	for i := 1; i <= 5; i++ {
		if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); err != nil {
			t.Fatalf("第 %d 次文本回复不应失败: %v", i, err)
		}
		if got := f.bodyAt(i - 1).MsgSeq; got != i {
			t.Fatalf("seq 应从 1 连续到 5，第 %d 条实际 %d", i, got)
		}
	}
	if err := s.Reply(ctx, ev, host.Reply{Content: "hi"}); !errors.Is(err, ErrPassiveExhausted) {
		t.Fatalf("第 6 次应耗尽，实际 err=%v", err)
	}
}

// 拆分后第二条失败时，错误必须说明第一条已经发出去了——插件据此决定
// 要不要补发，而不是把两条都重发一遍。
func TestReplySecondPartFailureReportsPartialSend(t *testing.T) {
	s, f := imageSender(t)
	f.failSendAt = 2

	err := s.Reply(context.Background(), msgEvent(), host.Reply{
		Content: "看图",
		Image:   host.ImageFromBytes(tinyPNG()),
	})
	if err == nil {
		t.Fatal("第二条失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "前 1 条已发出") {
		t.Fatalf("错误应说明已发出的条数，实际: %v", err)
	}
	if n := f.callCount(); n != 2 {
		t.Errorf("应尝试两条（一成一败），实际 %d", n)
	}
}

// 构造 Image 不产生 IO：路径可以稍后才有文件。
func TestImageFromFileReadsAtSendTime(t *testing.T) {
	s, f := imageSender(t)
	path := filepath.Join(t.TempDir(), "chart.png")
	img := host.ImageFromFile(path)

	if err := os.WriteFile(path, tinyPNG(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reply(context.Background(), msgEvent(), host.Reply{Image: img}); err != nil {
		t.Fatalf("发本地图不应失败: %v", err)
	}
	up := f.uploadAt(0)
	if up.FileName != "chart.png" {
		t.Errorf("文件名应取路径 basename，实际 %q", up.FileName)
	}
	if up.FileData == "" {
		t.Error("本地文件应走 file_data")
	}
}

func TestReplyImageFileMissingFailsBeforeUpload(t *testing.T) {
	s, f := imageSender(t)
	err := s.Reply(context.Background(), msgEvent(), host.Reply{
		Image: host.ImageFromFile(filepath.Join(t.TempDir(), "nope.png")),
	})
	if err == nil || !strings.Contains(err.Error(), "读取图片") {
		t.Fatalf("文件不存在时应报读取失败，实际 err=%v", err)
	}
	if n := f.uploadCount(); n != 0 {
		t.Errorf("读取失败时不该上传，实际 %d 次", n)
	}
}

// msg_type=7 却没有图，是插件写错了；发出去只会被平台拒。
func TestReplyMediaTypeWithoutImageRejected(t *testing.T) {
	s, f := imageSender(t)
	err := s.Reply(context.Background(), msgEvent(), host.Reply{MsgType: 7})
	if err == nil || !strings.Contains(err.Error(), "必须带图片") {
		t.Fatalf("应提示 msg_type=7 必须带图，实际 err=%v", err)
	}
	if n := f.callCount(); n != 0 {
		t.Errorf("不该发消息，实际 %d 次", n)
	}
}

// 主动消息的日预算按天计、比被动窗口稀缺，所以先扣预算再上传：
// 额度用尽时不该再浪费一次上传。
func TestActiveImageBudgetCheckedBeforeUpload(t *testing.T) {
	f := &fakeAPI{}
	s := NewSender(f, slog.New(slog.DiscardHandler), ActivePolicy{
		Enabled:      true,
		Plugins:      map[string]bool{"digest": true},
		DailyPerPeer: 1,
	}, newUsageStore(t))
	p := s.ForPlugin("digest")
	ctx := context.Background()

	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Image: host.ImageFromBytes(tinyPNG())}); err != nil {
		t.Fatalf("主动发图不应失败: %v", err)
	}
	if body := f.bodyAt(0); body.Media == nil || body.MsgID != "" {
		t.Errorf("主动图片消息应带 media 且无 msg_id，实际 %+v", body)
	}

	if err := p.SendActive(ctx, host.SceneC2C, "u1", host.Reply{Image: host.ImageFromBytes(tinyPNG())}); !errors.Is(err, ErrActiveQuota) {
		t.Fatalf("第二次应超出日预算，实际 err=%v", err)
	}
	if n := f.uploadCount(); n != 1 {
		t.Errorf("额度用尽后不该再上传，实际上传 %d 次", n)
	}
}

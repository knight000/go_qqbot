package core

import (
	"context"
	"testing"

	"github.com/knight000/go_qqbot/internal/host"
)

// TestImageOnlyFailureSendsFallbackText 钉住 #25 要的"降级"：纯图片回复
// 一条都没发出去时，用户那边什么都看不到。核心必须在同一 msg_id 上补一句
// 文本说明——否则插件以为发出去了，用户以为机器人没理他。
func TestImageOnlyFailureSendsFallbackText(t *testing.T) {
	s, f := imageSender(t)
	f.failSendAt = 1 // 第一次 SendMessage（图片本身）失败

	if err := s.Reply(context.Background(), msgEvent(), host.Reply{Image: host.ImageFromBytes(tinyPNG())}); err == nil {
		t.Fatal("图片发送失败应返回错误（dispatch 据此记 Warn）")
	}

	if n := f.callCount(); n != 2 {
		t.Fatalf("图片失败后应补发一条文本，共 2 次发送，实际 %d 次", n)
	}
	last := f.bodies[len(f.bodies)-1]
	if last.Content != imageFailedText {
		t.Fatalf("补发内容应为 %q，实际 %q", imageFailedText, last.Content)
	}
	if last.MsgType != 0 {
		t.Fatalf("补发必须是纯文本（msg_type=0），实际 %d", last.MsgType)
	}
	// 补发同样是一次被动回复，必须带同一个 msg_id：换成新 msg_id 平台不认。
	if last.MsgID != "m1" {
		t.Fatalf("补发应复用同一 msg_id，实际 %q", last.MsgID)
	}
}

// TestTextThenImageFailureDoesNotSendFallback 是上一条的反向守卫：
// 文本已经发出去时用户已经看到内容，再补一句只是噪音。
func TestTextThenImageFailureDoesNotSendFallback(t *testing.T) {
	s, f := imageSender(t)
	f.failSendAt = 2 // 第 1 次（文本）成功，第 2 次（图片）失败

	err := s.Reply(context.Background(), msgEvent(), host.Reply{
		Content: "查询结果：",
		Image:   host.ImageFromBytes(tinyPNG()),
	})
	if err == nil {
		t.Fatal("图片失败应返回错误")
	}
	if n := f.callCount(); n != 2 {
		t.Fatalf("前面已有文本成功发出时不应补发，实际发送 %d 次", n)
	}
}

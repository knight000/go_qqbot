package core

import (
	"context"

	"github.com/knight000/go_qqbot/internal/api"
)

// stateTrackingAPI 给所有出站套一层状态观测：只看成败，不改任何发送语义。
//
// 为什么用装饰器而不是在每个返回点调用：被动回复、主动消息、富媒体上传是三条
// 独立路径（还有拆成两条消息的情形），任何一处漏接就等于"这条通道的鉴权失败
// 不算数"，而 failing 的判据只有这一个信号。
type stateTrackingAPI struct {
	inner  messageAPI
	notify func(context.Context, error)
}

func (s stateTrackingAPI) SendMessage(ctx context.Context, scene, peer string, body api.MessageBody) error {
	err := s.inner.SendMessage(ctx, scene, peer, body)
	s.notify(ctx, err)
	return err
}

func (s stateTrackingAPI) UploadMedia(ctx context.Context, scene, peer string, up api.MediaUpload) (api.MediaUploadResult, error) {
	res, err := s.inner.UploadMedia(ctx, scene, peer, up)
	s.notify(ctx, err)
	return res, err
}

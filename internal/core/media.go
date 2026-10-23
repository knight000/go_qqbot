package core

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/knight000/go_qqbot/internal/api"
	"github.com/knight000/go_qqbot/pkg/host"
)

// uploadImage 上传一张图片并返回 file_info。
//
// 上传是发图的必经一步：平台要求先拿到 file_info，再用 msg_type=7 发送。
// 单聊与群聊的上传接口互相隔离（群文件不能发到单聊），所以 scene/peer
// 必须原样透传，不能在这里"猜"场景。
func (s *Sender) uploadImage(ctx context.Context, scene host.Scene, peer string, img host.Image) (string, error) {
	up, err := buildMediaUpload(img)
	if err != nil {
		return "", err
	}
	res, err := s.api.UploadMedia(ctx, string(scene), peer, up)
	if err != nil {
		return "", fmt.Errorf("上传图片: %w", err)
	}
	return res.FileInfo, nil
}

// buildMediaUpload 把 host.Image 收口成一次上传请求。
//
// 三种来源只有两条路：URL 交给平台下载转存，文件和内存字节都读成
// 字节走二进制直传。分片上传（>5 MB 时唯一可行的路）没有实现。
func buildMediaUpload(img host.Image) (api.MediaUpload, error) {
	switch img.Source() {
	case host.ImageSourceURL:
		u, err := parseImageURL(img.URL())
		if err != nil {
			return api.MediaUpload{}, err
		}
		return api.MediaUpload{FileType: api.MediaImage, URL: u}, nil

	case host.ImageSourceFile:
		path := strings.TrimSpace(img.Path())
		if path == "" {
			return api.MediaUpload{}, errors.New("图片路径为空")
		}
		// 先看大小再读：把 2 GB 的文件读进内存才发现超限，代价是 OOM。
		if info, err := os.Stat(path); err == nil && info.Size() > api.MaxInlineMediaBytes {
			return api.MediaUpload{}, tooLargeErr(info.Size())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return api.MediaUpload{}, fmt.Errorf("读取图片: %w", err)
		}
		return inlineUpload(data, filepath.Base(path))

	case host.ImageSourceBytes:
		return inlineUpload(img.Data(), "")

	default:
		return api.MediaUpload{}, errors.New("没有可发送的图片")
	}
}

// inlineUpload 组装二进制直传请求。
//
// 5 MB 是平台硬上限，超过只能走分片上传；在本地就判掉，别把 20 MB 的图
// base64 成 27 MB 的 JSON 再被平台拒绝。
func inlineUpload(data []byte, name string) (api.MediaUpload, error) {
	if len(data) == 0 {
		return api.MediaUpload{}, errors.New("图片内容为空")
	}
	if len(data) > api.MaxInlineMediaBytes {
		return api.MediaUpload{}, tooLargeErr(int64(len(data)))
	}
	// 平台靠 file_name 的扩展名判格式，名字不对会直接 850019。
	name, err := imageName(name, data)
	if err != nil {
		return api.MediaUpload{}, err
	}
	return api.MediaUpload{
		FileType: api.MediaImage,
		FileData: base64.StdEncoding.EncodeToString(data),
		FileName: name,
	}, nil
}

// tooLargeErr 统一「超过直传上限」的措辞：两种情况（文件、内存字节）都
// 要指向同一个出路——压缩，或者改用由平台下载的 URL 来源。
func tooLargeErr(size int64) error {
	return fmt.Errorf(
		"图片 %d 字节超过直传上限 %d 字节（更大的图需要分片上传，当前未实现；请压缩或改用 ImageFromURL）",
		size, api.MaxInlineMediaBytes)
}

// parseImageURL 校验 URL 来源。URL 由平台侧下载，本机不发起请求，
// 所以这里只拦"明显不可能成功"的输入，不做域名白名单。
func parseImageURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("图片 URL 非法: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("图片 URL 必须以 http(s):// 开头: %q", raw)
	}
	return u.String(), nil
}

// imageName 给出平台能认的文件名：已知扩展名沿用，未知的按内容嗅探，
// `.jpeg` 收敛成文档里写的 `.jpg`。
func imageName(name string, data []byte) (string, error) {
	switch ext := strings.ToLower(filepath.Ext(name)); ext {
	case ".png", ".gif", ".webp", ".bmp":
		return name, nil
	case ".jpg", ".jpeg":
		return strings.TrimSuffix(name, filepath.Ext(name)) + ".jpg", nil
	}
	sniffed, ok := imageNameByContent(data)
	if !ok {
		return "", errors.New("无法识别图片格式（支持 png/jpg/gif/webp/bmp）")
	}
	return sniffed, nil
}

// imageNameByContent 按文件头猜扩展名，给内存字节补一个平台能认的文件名。
func imageNameByContent(data []byte) (string, bool) {
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	switch http.DetectContentType(head) {
	case "image/png":
		return "image.png", true
	case "image/jpeg":
		return "image.jpg", true
	case "image/gif":
		return "image.gif", true
	case "image/webp":
		return "image.webp", true
	case "image/bmp":
		return "image.bmp", true
	}
	return "", false
}

// replySummary 是轨迹里显示的"发了什么"。图片没有正文，得把来源写出来，
// 否则 [debug] 里只能看到一条空内容。
func replySummary(msg host.Reply) string {
	if !msg.Image.Valid() {
		return msg.Content
	}
	img := msg.Image
	switch img.Source() {
	case host.ImageSourceFile:
		return "[图片] " + filepath.Base(img.Path())
	case host.ImageSourceURL:
		return "[图片] " + img.URL()
	default:
		return fmt.Sprintf("[图片] 内存 %d 字节", len(img.Data()))
	}
}

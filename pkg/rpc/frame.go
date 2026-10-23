// Package rpc 是核心与插件子进程之间的线协议（ADR-0005 决策 4 / 9）。
//
// 两端各持一个 Conn：它同时是客户端与服务端——核心既调用 plugin.* 又服务
// host.*，插件反过来。只有这样才能让 Init 里的 Host 调用（典型如 Config()）
// 不与自己阻塞：读循环永远在跑，等应答的协程只是挂在自己的 pending 通道上。
//
// 帧格式：
//
//	+---------+---------+--------------------+--------------------+
//	| N (4B)  | H (4B)  | 头部 H 字节 (JSON)  | 正文 N-4-H 字节     |
//	+---------+---------+--------------------+--------------------+
//
// N 数的是「它自己之后」的全部字节数，所以 N = 4 + H + len(正文)。读侧的
// 第一步是先读 4 字节拿到 N，这个偏移定义错了就永远对不上，因此把前缀
// 排除在 N 之外——N 与「接下来还要读多少」是同一个数，没有第二层换算。
//
// 正文槽只有一个，它承载 Image.data：Renderer.Markdown 返回 1–3 MB 的 PNG，
// 单帧直传、不做 base64。把 PNG 塞进 JSON 会多出 4/3 的膨胀与一份同样大小的
// 临时字符串，而这里两端的常驻内存预算只有几十 MB（§3.8 的账）。正文槽
// 不参与多路复用——每条帧最多一张图，与 Reply 只有一个 Image 同构。
//
// 这个方法表与 host.Plugin / host.Host 一一对应，见 method.go；错误只走数值
// 码不走字符串，见 errors.go。
package rpc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// MaxFrameBytes 是单帧上限（16 MiB）。最坏情况是 Renderer.Markdown 的
	// 3 MB PNG 加上头部与 base64 过的 Event.Raw，量级差三个数量级；设上限是
	// 为了让一个错乱的长度前缀（半包、串流、写错端）立刻报错而不是先
	// make 一个几十 GB 的切片把进程 OOM 掉。
	MaxFrameBytes = 16 << 20

	// maxHeaderBytes 是头部 JSON 的上限。头里最大的是 Event 的 Raw（≤32 KiB，
	// base64 后 43 KiB）与 Query 的参数，1 MiB 已是三个数量级的余量。
	maxHeaderBytes = 1 << 20
)

var (
	// ErrFrameTooLarge 表示头部或整帧超过上限。它是本地错误，不过通道。
	ErrFrameTooLarge = errors.New("rpc: 帧超过上限")
	// ErrBadFrame 表示长度前缀自相矛盾（头部长度大于整帧、帧短于最小长度）。
	ErrBadFrame = errors.New("rpc: 帧结构不合法")
)

// frame 是一帧：JSON 头部 + 至多一个正文槽。
type frame struct {
	header []byte
	body   []byte
}

// writeFrame 把一帧写进 w。整帧拼在一个缓冲区里一次写出：
// 长度前缀与内容必须原子地落进管道，否则并发写入会在帧中间交错，
// 读侧从此永久失步（而失步的表现是「某天开始所有请求都超时」）。
func writeFrame(w io.Writer, f frame) error {
	if len(f.header) > maxHeaderBytes {
		return fmt.Errorf("%w: 头部 %d 字节 > %d", ErrFrameTooLarge, len(f.header), maxHeaderBytes)
	}
	total := 4 + len(f.header) + len(f.body)
	if total > MaxFrameBytes {
		return fmt.Errorf("%w: 帧 %d 字节 > %d", ErrFrameTooLarge, total, MaxFrameBytes)
	}
	buf := make([]byte, 4+total)
	binary.BigEndian.PutUint32(buf[0:4], uint32(total))
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(f.header)))
	copy(buf[8:], f.header)
	copy(buf[8+len(f.header):], f.body)
	return writeFull(w, buf)
}

// writeFull 处理短写。io.Writer 允许「写了 n < len(p) 且 err == nil」，
// 而 io.Copy 之外的代码很少检查它——这里的失败必须是错误，不能是静默。
func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// readFrame 读一帧。
//
// 对端在帧边界上正常关闭 → io.EOF（调用方据此收摊，不算故障）；
// 读到一半对端就没了 → io.ErrUnexpectedEOF（这是故障，必须冒出去）。
// 两者都用 io.ReadFull，所以「半包」不会伪装成「空帧」。
func readFrame(r io.Reader) (frame, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return frame{}, err
	}
	total := binary.BigEndian.Uint32(prefix[:])
	// 最小合法帧是 4 字节头部长度 + "{}"。
	if total < 6 || total > MaxFrameBytes {
		return frame{}, fmt.Errorf("%w: 声明长度 %d", ErrBadFrame, total)
	}
	payload := make([]byte, total)
	if _, err := io.ReadFull(r, payload); err != nil {
		return frame{}, err
	}
	headerLen := binary.BigEndian.Uint32(payload[0:4])
	if headerLen < 2 || int(headerLen) > len(payload)-4 {
		return frame{}, fmt.Errorf("%w: 头部 %d 字节，帧内可用 %d", ErrBadFrame, headerLen, len(payload)-4)
	}
	return frame{
		header: payload[4 : 4+headerLen],
		body:   payload[4+headerLen:],
	}, nil
}

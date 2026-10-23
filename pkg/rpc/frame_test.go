package rpc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// TestFrameRoundTrip 是最基础的一条：头部与正文都要原样回来。
//
// 长度前缀的写法一旦算错（把前缀自己算进 N、或把 H 写成含前缀），
// 现象是「能连上、但第一帧之后全部错位」。所以这里连边界长度也一起测。
func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		header []byte
		body   []byte
	}{
		{"空正文", []byte(`{"id":1}`), nil},
		{"短正文", []byte(`{"id":2}`), []byte("png")},
		{"二进制正文", []byte(`{"id":3}`), []byte{0x00, 0xff, 0x00, 0x7f}},
		{"空正文切片", []byte(`{"id":4}`), []byte{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeFrame(&buf, frame{header: c.header, body: c.body}); err != nil {
				t.Fatalf("写帧: %v", err)
			}
			got, err := readFrame(&buf)
			if err != nil {
				t.Fatalf("读帧: %v", err)
			}
			if !bytes.Equal(got.header, c.header) {
				t.Errorf("头部 = %q，期望 %q", got.header, c.header)
			}
			if !bytes.Equal(got.body, c.body) {
				t.Errorf("正文 = %d 字节，期望 %d 字节", len(got.body), len(c.body))
			}
			if buf.Len() != 0 {
				t.Errorf("读完还剩 %d 字节：长度前缀算错了", buf.Len())
			}
		})
	}
}

// TestFrameLargeBody 是验收要求的 3 MB 单帧往返。
//
// 断言哈希而不是长度：长度对得上而内容被裁掉一大段的情形，只有逐字节
// 比较才抓得到。
func TestFrameLargeBody(t *testing.T) {
	const size = 3 << 20
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i*31 + 7)
	}
	var buf bytes.Buffer
	if err := writeFrame(&buf, frame{header: []byte(`{"id":9}`), body: body}); err != nil {
		t.Fatalf("写 3 MB 帧: %v", err)
	}
	got, err := readFrame(&buf)
	if err != nil {
		t.Fatalf("读 3 MB 帧: %v", err)
	}
	if len(got.body) != size {
		t.Fatalf("正文 %d 字节，期望 %d", len(got.body), size)
	}
	if !bytes.Equal(got.body, body) {
		t.Fatal("3 MB 正文内容不一致")
	}
}

// TestFrameHalfPacket：对端读到一半就没了，必须报错而不是返回一个短帧。
func TestFrameHalfPacket(t *testing.T) {
	t.Run("前缀只到一半", func(t *testing.T) {
		_, err := readFrame(bytes.NewReader([]byte{0x00, 0x00}))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v，期望 io.ErrUnexpectedEOF", err)
		}
	})
	t.Run("正文只到一半", func(t *testing.T) {
		var buf bytes.Buffer
		if err := writeFrame(&buf, frame{header: []byte(`{"id":1}`), body: bytes.Repeat([]byte{7}, 64)}); err != nil {
			t.Fatal(err)
		}
		full := buf.Bytes()
		_, err := readFrame(bytes.NewReader(full[:len(full)-10]))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v，期望 io.ErrUnexpectedEOF", err)
		}
	})
}

// TestFrameBadLength：长度前缀自相矛盾必须立刻失败。
//
// 这条的另一种走法是「先 make 一个巨大切片」——一个错乱的前缀就能把进程
// OOM 掉，所以长度校验放在分配之前。
func TestFrameBadLength(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{"零长度", []byte{0, 0, 0, 0}},
		{"小于最小帧", []byte{0, 0, 0, 5, 0, 0, 0, 1}},
		{"超过上限", []byte{0xff, 0xff, 0xff, 0xff}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := readFrame(bytes.NewReader(c.input))
			if !errors.Is(err, ErrBadFrame) {
				t.Fatalf("err = %v，期望 ErrBadFrame", err)
			}
		})
	}
	t.Run("头部长度超出帧内可用", func(t *testing.T) {
		// N=8（帧内 8 字节），头部声明 100 字节。
		input := []byte{0, 0, 0, 8, 0, 0, 0, 100, 'a', 'b', 'c', 'd'}
		_, err := readFrame(bytes.NewReader(input))
		if !errors.Is(err, ErrBadFrame) {
			t.Fatalf("err = %v，期望 ErrBadFrame", err)
		}
	})
}

// TestFrameHeaderTooLargeReadSide：头部上限必须在**读**侧成立。
//
// 只在写侧校验等于没有校验：上限是给「对端说了算」的字段兜底的，而写侧
// 是本单位。帧总长在 32 MiB 之内、头部却声明 8 MiB 的帧，如果不判，
// 一次 8 MiB 的 make 就发生了——这正是上限要防的那件事。
func TestFrameHeaderTooLargeReadSide(t *testing.T) {
	h := maxHeaderBytes + 1
	total := 4 + h // 4 字节头部长度 + 头部本身，正文为 0
	buf := make([]byte, 4+total)
	binary.BigEndian.PutUint32(buf[0:4], uint32(total))
	binary.BigEndian.PutUint32(buf[4:8], uint32(h))

	if _, err := readFrame(bytes.NewReader(buf)); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("err = %v，期望 ErrBadFrame（头部超过 %d）", err, maxHeaderBytes)
	}
}

// TestFrameTooLarge 断言超上限在**写**侧就失败，不等读侧发现。
func TestFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	err := writeFrame(&buf, frame{header: []byte(`{"id":1}`), body: make([]byte, MaxFrameBytes)})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v，期望 ErrFrameTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("超限的帧仍然写出去 %d 字节", buf.Len())
	}
}

// TestFrameCleanEOF 区分「对端在帧边界上正常退出」与「读到一半断了」：
// 前者是子进程下班（Run 返回 nil），后者是故障。
func TestFrameCleanEOF(t *testing.T) {
	_, err := readFrame(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v，期望 io.EOF", err)
	}
}

// TestWriteFullShortWrite：io.Writer 允许短写，静默接受会让帧缺一截。
func TestWriteFullShortWrite(t *testing.T) {
	w := &chunkWriter{max: 3}
	if err := writeFull(w, []byte("0123456789")); err != nil {
		t.Fatalf("writeFull: %v", err)
	}
	if w.String() != "0123456789" {
		t.Fatalf("写出去的是 %q", w.String())
	}

	w = &chunkWriter{max: 0}
	if err := writeFull(w, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v，期望 io.ErrShortWrite", err)
	}
}

type chunkWriter struct {
	bytes.Buffer
	max int
}

func (c *chunkWriter) Write(p []byte) (int, error) {
	if c.max <= 0 {
		return 0, nil
	}
	if len(p) > c.max {
		p = p[:c.max]
	}
	return c.Buffer.Write(p)
}

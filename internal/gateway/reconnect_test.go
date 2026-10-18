package gateway

import (
	"errors"
	"testing"
)

// 没有活连接时 Reconnect 必须给出可识别的错误，而不是静静地成功——
// 否则 /reset reconnect 会回一句"已请求重连"，而实际上什么都没发生。
func TestReconnectWithoutConnection(t *testing.T) {
	c := New(Options{})
	if err := c.Reconnect(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("应返回 ErrNotConnected，实际: %v", err)
	}
}

// 手动标记必须被消费一次就清掉：残留的标记会把之后一次真实故障
// 误报成"手动重连"，从而跳过退避、在平台侧连续重连。
func TestTakeManualIsOneShot(t *testing.T) {
	c := New(Options{})
	c.mu.Lock()
	c.manual = true
	c.mu.Unlock()

	if !c.takeManual() {
		t.Fatal("第一次应读到手动标记")
	}
	if c.takeManual() {
		t.Fatal("第二次不应再读到")
	}
}

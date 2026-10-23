package pluginproc

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/knight000/go_qqbot/pkg/rpc"
)

// runtimeProc 是「一个子进程 + 一条连接」的一次生命周期。
//
// 进程被重启时旧的 runtimeProc 整个作废、新的另起一个：把「代」显式化,
// 是为了让旧进程的监工协程没法误伤新进程（它只碰自己那一份 cmd/conn）。
type runtimeProc struct {
	cmd    *exec.Cmd
	conn   *rpc.Conn
	stderr *lineLogger
	// exited 在 cmd.Wait 返回后关闭；exitErr 在此之后才可读（close 提供
	// happens-before）。
	exited  chan struct{}
	exitErr error
}

// startProcess 起子进程并把 stdio 接成一条 rpc.Conn。
//
// stdout 是协议通道、stderr 是日志通道（rpcsdk 的约定）：所以 stdout 走帧，
// stderr 逐行进 slog——插件的日志绝不过 RPC（决策 4）。
func startProcess(bin string, reg *rpc.Registry, log *slog.Logger) (*runtimeProc, error) {
	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("接插件 stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("接插件 stdout: %w", err)
	}
	// stderr 用 io.Writer 而不是 StderrPipe：exec 会用一个内部协程把输出抄进
	// Writer，Wait 会等抄完——这样退出时的最后几行不会丢，也不会有"Wait 与
	// 读管道并发"的竞态。
	stderr := newLineLogger(log)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s: %w", bin, err)
	}

	r := &runtimeProc{
		cmd:    cmd,
		conn:   rpc.NewConn(stdout, stdin, rpc.Options{Registry: reg, Logger: log}),
		stderr: stderr,
		exited: make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		// Wait 返回时 stderr 的拷贝协程已经结束，这时 flush 不会有并发写。
		stderr.flush()
		r.exitErr = err
		close(r.exited)
	}()
	return r, nil
}

// shutdown 收掉这一代进程：先断连接（插件读到 EOF 会自己退），再兜底杀。
//
// 只用于「握手失败」这类还没进入正常运行期的收尾；正常运行期的三级关闭在
// Close 里（RPC close → SIGTERM → Kill，每级带超时）。
func (r *runtimeProc) shutdown() {
	_ = r.conn.Close()
	select {
	case <-r.exited:
		return
	case <-time.After(time.Second):
	}
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	<-r.exited
}

// maxStderrLine 是单行 stderr 的上限（64 KiB）。超过就切断当一行打出去：
// 一个插件有可能把整份 JSON 打进一行，不能让核心为它攒着一个大缓冲区。
const maxStderrLine = 64 << 10

// lineLogger 把插件的 stderr 按行重放进 slog。
//
// 为什么按行：插件的日志是"一条一事"，多行事件打散会把堆栈与它的上下文
// 拆到两条记录里，检索时对不上。加 plugin=<name> 由调用方在 logger 上带好了。
type lineLogger struct {
	log *slog.Logger

	mu  sync.Mutex
	buf []byte
}

func newLineLogger(log *slog.Logger) *lineLogger { return &lineLogger{log: log} }

// Write 收一段字节，把其中完整的行逐条打出去，剩下的留在缓冲里等下一次。
func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.emit(l.buf[:i])
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > maxStderrLine {
		l.emit(l.buf)
		l.buf = l.buf[:0]
	}
	return len(p), nil
}

// flush 把没有换行结尾的残行打出去（进程退出时调用）。
func (l *lineLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.emit(l.buf)
		l.buf = l.buf[:0]
	}
}

func (l *lineLogger) emit(line []byte) {
	// 去掉 Windows 的 \r：两台机器上同一条日志应当逐字相同。
	line = bytes.TrimRight(line, "\r")
	if len(line) == 0 {
		return
	}
	l.log.Info(string(line))
}

// 断言：exec 只要求 io.Writer，别不小心把它换成更窄的东西。
var _ io.Writer = (*lineLogger)(nil)

// Package buildinfo 汇总二进制的版本信息，供启动日志、/ping 与 /reload 使用。
//
// commit 默认来自 go build 自动嵌入的 VCS 信息（vcs.revision / vcs.time /
// vcs.modified），因此不需要打 tag 也能回答"现在跑的是不是最新代码"：
// 工作区有未提交改动时会带 +dirty 后缀。
//
// 版本号可用 -ldflags 注入：
//
//	go build -ldflags "-X github.com/knight000/go_qqbot/internal/buildinfo.Version=v1.2.3"
package buildinfo

import (
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Version 是发布版本号，默认 dev；构建时可由 -ldflags 覆盖。
var Version = "dev"

// RestartEnv 是 /reload 重新执行自身时写入的环境变量。
// 启动日志据此标注"本次是重载启动"，bootnotice 据此跳过崩溃抑制窗口。
const RestartEnv = "QQBOT_RESTARTED"

type Info struct {
	Version  string // dev / v1.2.3
	Commit   string // 短哈希，脏工作区带 +dirty
	CommitAt string // 提交时间（本地时区）
	Platform string // windows/amd64
	GoVer    string // runtime.Version()
}

var (
	once sync.Once
	info Info
)

// Get 返回当前二进制信息。解析失败时只有 Version/Platform/GoVer 可用。
func Get() Info {
	once.Do(func() {
		info = Info{
			Version:  Version,
			Platform: runtime.GOOS + "/" + runtime.GOARCH,
			GoVer:    runtime.Version(),
		}
		bi, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		if info.Version == "dev" && isReleaseVersion(bi.Main.Version) {
			info.Version = bi.Main.Version
		}

		var rev, at string
		dirty := false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				at = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if len(rev) > 8 {
			rev = rev[:8]
		}
		if rev != "" && dirty {
			rev += "+dirty"
		}
		info.Commit = rev
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			info.CommitAt = t.Local().Format("01-02 15:04")
		}
	})
	return info
}

// String 是给人看的一行版本标识：dev 2ce990b / v1.0.0 2ce990b+dirty。
func (i Info) String() string {
	if i.Commit == "" {
		return i.Version
	}
	return i.Version + " " + i.Commit
}

// RestartedByReload 报告本进程是否由 /reload 拉起。
func RestartedByReload() bool { return os.Getenv(RestartEnv) == "1" }

// isReleaseVersion 过滤 go build 自动生成的伪版本。
//
// 本地构建时 Main.Version 形如 v0.0.0-20261017073004-cc908d875f31+dirty，
// 直接拿来展示会和 commit 重复，还长得没法看。只有真正打 tag 安装
// （go install module@v1.2.3）时它才是干净的版本号。
func isReleaseVersion(v string) bool {
	if v == "" || v == "(devel)" {
		return false
	}
	if strings.HasPrefix(v, "v0.0.0-") || strings.Contains(v, "+dirty") {
		return false
	}
	return true
}

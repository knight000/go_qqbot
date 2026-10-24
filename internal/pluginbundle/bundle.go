// Package pluginbundle 是核心二进制里内嵌的出厂插件副本。
//
// 内嵌副本是**释放源与对齐源**（ADR-0005 决策 11）：运行期的事实源永远是
// <datadir>/plugins/bin/ 里的文件——在就是装着，不在就是没装。核心启动期
// 不做无条件的释放，而是用内嵌副本做一次「只升不降」的对齐（#52）：内嵌
// 版本更高、或磁盘插件的 api_version 与核心不一致才替换，缺文件时补回。
// 那套判定住在 internal/pluginalign，本包只提供原语。
//
// 释放全部缺失文件仍走显式命令 `qqbot init --plugins`（#42）：本包提供
// Files / Names / Versions / Extract 四个原语，不碰装配。
//
// 产物位置是 internal/pluginbundle/dist/<goos>_<goarch>/ 而不是仓库根的
// dist/：go:embed 的模式不得引用包目录之外的文件，这是唯一可行的位置
// （ADR-0005 决策 11 已按此更正）。每个平台一个文件 + build tag，别的平台
// 落到 bundle_other.go 的空实现上，编译与 go test 都不炸。
package pluginbundle

import (
	"errors"
	"io/fs"
	"sort"
	"strings"
)

// ErrUnavailable 表示本平台的构建里没有出厂插件。
//
// 它不是故障：从源码自己编一个别的平台（或本地没跑 tools/build plugins）
// 都会得到这个结果。可读的一句话是给 CLI 直接打给用户看的。
var ErrUnavailable = errors.New(
	"本平台没有出厂插件：内嵌副本只随 windows/amd64、linux/amd64、linux/arm64 发布")

// Files 返回本平台的出厂插件文件集；本平台没有时返回一个空 FS。
//
// 返回的 FS 根目录下就是插件文件本身（不含 dist/ 前缀），所以调用方不必
// 知道 <goos>_<goarch> 这段路径。
func Files() fs.FS {
	if embeddedFS == nil {
		return emptyFS{}
	}
	return embeddedFS
}

// VersionsFile 是版本清单在产物目录里的文件名。
//
// 下划线前缀是必须的：namesOf 会跳过 `.` 与 `_` 开头的文件（那是给 .gitkeep
// 与 Go 工具链约定留的），而 //go:embed all: 会把它们也嵌进来。于是清单既
// 随二进制发布，又不会被 Names / Extract 当成一个插件。
const VersionsFile = "_versions"

// Versions 返回内嵌副本里每个插件自报的版本（name → version）。
//
// 清单缺失、为空或格式坏时返回空 map：版本对齐是尽力而为的能力，不能因为
// 一份清单读不动就让机器人起不来。半份清单同样作废（见 versionsOf），
// 免得"某些插件没版本"被静默当成"不用对齐"。
func Versions() map[string]string {
	return versionsOf(embeddedFS)
}

// versionsOf 解析 `<name>=<version>` 逐行清单。允许空行与 # 注释。
func versionsOf(fsys fs.FS) map[string]string {
	out := map[string]string{}
	if fsys == nil {
		return out
	}
	data, err := fs.ReadFile(fsys, VersionsFile)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, ver, ok := strings.Cut(line, "=")
		name, ver = strings.TrimSpace(name), strings.TrimSpace(ver)
		if !ok || name == "" || ver == "" {
			return map[string]string{}
		}
		out[name] = ver
	}
	return out
}

// Names 返回本平台出厂插件的名字，按字典序。
//
// 占位文件（.gitkeep）与下划线前缀不算插件：前者是给 git 保住空目录用的，
// 后者是 Go 工具链忽略的约定。本平台没有出厂插件时返回 nil。
func Names() []string {
	names, err := namesOf(embeddedFS)
	if err != nil {
		// 内嵌 FS 读不动只可能是二进制本身有问题；启动路径不该因此失败，
		// 表现成"没有出厂插件"即可（真正的故障会由 Extract 报出来）。
		return nil
	}
	return names
}

// namesOf 列出 FS 根目录下的插件文件名。
func namesOf(fsys fs.FS) ([]string, error) {
	if fsys == nil {
		return nil, nil
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// mustSub 把内嵌 FS 收窄到 dist 目录。目录由 //go:embed 保证存在，
// 失败只可能是编译期那行指令写错了。
func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("pluginbundle: 内嵌目录 " + dir + ": " + err.Error())
	}
	return sub
}

// emptyFS 是"本平台没有出厂副本"时的 Files() 返回值：可以安全地遍历，
// 但没有任何文件。返回 nil 会让调用方（含 #42 的 init）多一条空指针路径。
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

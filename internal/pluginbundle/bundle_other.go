//go:build !((windows && amd64) || (linux && amd64) || (linux && arm64))

package pluginbundle

import "io/fs"

// embeddedFS 为 nil：本平台没有出厂插件。
//
// 这不是"忘了写 build tag"——darwin/arm64 这类平台要能从源码直接
// `go build ./...` 与 `go test ./...`，只是没有可释放的产物。Extract 会
// 返回 ErrUnavailable，Files 给一个空 FS，Names 为空。
var embeddedFS fs.FS

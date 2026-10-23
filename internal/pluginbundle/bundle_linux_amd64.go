//go:build linux && amd64

package pluginbundle

import (
	"embed"
	"io/fs"
)

//go:embed all:dist/linux_amd64
var platformDist embed.FS

// embeddedFS 是本平台的出厂插件副本，根目录即 plugins/bin 的内容。
var embeddedFS fs.FS = mustSub(platformDist, "dist/linux_amd64")

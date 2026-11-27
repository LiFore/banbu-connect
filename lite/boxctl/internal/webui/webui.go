// Package webui 内嵌前端构建产物（web/ 下 `npm run build` 输出到 dist/）。
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS 返回以 dist 为根的文件系统。
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist 由 go:embed 保证存在
	}
	return sub
}

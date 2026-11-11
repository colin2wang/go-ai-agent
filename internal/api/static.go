package api

import (
	"embed"
	"io/fs"
)

// webdist 内嵌前端构建产物（web/dist 构建后复制到此目录；仓库内为占位页）。
//
//go:embed all:webdist
var webdist embed.FS

// AssetsFS 返回前端静态资产（供 Wails AssetServer 使用）。
func AssetsFS() fs.FS {
	sub, err := fs.Sub(webdist, "webdist")
	if err != nil {
		panic(err) // embed 目录名固定，不可能失败
	}
	return sub
}

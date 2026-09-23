package api

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// webdist 内嵌前端构建产物（frontend/dist 构建后复制到此目录；仓库内为占位页）。
//
//go:embed all:webdist
var webdist embed.FS

// staticHandler 返回 SPA 静态资源处理器：命中文件则返回，未命中回退 index.html。
func staticHandler() http.Handler {
	sub, err := fs.Sub(webdist, "webdist")
	if err != nil {
		panic(err) // embed 目录名固定，不可能失败
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			// SPA 回退：未知路径返回入口页
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}

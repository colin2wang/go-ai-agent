package api

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// webdist 内嵌前端构建产物（web/dist 构建后复制到此目录；仓库内为占位页）。
//
//go:embed all:webdist
var webdist embed.FS

// staticHandler 返回 SPA 静态资源处理器：命中文件则返回，未命中回退 index.html。
//
// 缓存策略：/assets/* 由 vite 打内容哈希，可长期 immutable 缓存；
// 其余（尤其 index.html）必须 no-cache，否则前端重新构建后浏览器仍会沿用
// 旧的入口页与旧 JS（表现为“改了前端但页面行为没变”，例如 Markdown 不渲染）。
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
			p = "index.html"
			r.URL.Path = "/"
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

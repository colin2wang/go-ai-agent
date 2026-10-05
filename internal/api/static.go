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
// cspPolicy 前端页面的内容安全策略（对 DOMPurify 之外再补一道防线）。
// 说明：Vue 运行时与组件内联样式需要 style-src 'unsafe-inline'；
// 图片走 data:/blob:（本地预览与 LLM 多模态回传），故放开 img-src。
var cspPolicy = strings.Join([]string{
	"default-src 'self'",
	"script-src 'self'",
	"style-src 'self' 'unsafe-inline'",
	"img-src 'self' data: blob:",
	"font-src 'self' data:",
	"connect-src 'self'",
	"object-src 'none'",
	"base-uri 'self'",
	"frame-ancestors 'none'",
}, "; ")

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
		// HTML 页面统一加 CSP（静态资源同样下发，策略对它们无副作用）
		w.Header().Set("Content-Security-Policy", cspPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		fileServer.ServeHTTP(w, r)
	})
}

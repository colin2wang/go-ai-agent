// Command harness AI 工作流平台入口：
//   - 默认（无参数）：以 Web 形式运行，自动选端口并打开浏览器
//   - CLI 模式：harness <payslip.png> [config/config.yml] → 打印最终报告
//
// 配置为三层结构：config/config.yml（总配置）→ ai.yml（AI provider）→ features/*.yml（子功能）。
package main

import (
	"os"
	"path/filepath"
	"strings"
)

// version 由 build.ps1 的 ldflags 注入。
var version = "dev"

func main() {
	// 唯一的子命令形式：harness <图片路径> [config] → CLI 单图审读
	if len(os.Args) > 1 && isImagePath(os.Args[1]) {
		runCLI()
		return
	}
	// 其余情况（无参数 / serve / version 等）统一进入 Web 模式，
	// web.go 内部解析旗标（-addr/-config/-no-browser，未识别的忽略）。
	serveWebCmd()
}

// isImagePath 粗略判断参数是否为存在的图片文件路径。
func isImagePath(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".bmp":
		return true
	}
	return false
}

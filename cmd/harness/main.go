// Command harness 是 AI 工作流平台的唯一可执行入口，按第一个参数分派运行模式：
//
//	harness                        Web/API 服务（默认：自动选端口并打开浏览器）
//	harness serve [旗标]           同上，显式子命令（-addr / -config / -no-browser）
//	harness <图片路径> [总配置]     CLI 单图审读，打印最终报告
//	harness mq [-config p]         消息队列消费者（骨架）
//	harness cron [-interval 1h]    定时任务调度（骨架）
//	harness version                打印版本号
//
// 配置为三层结构：config/config.yml（总配置）→ ai.yml（AI provider）→ features/*.yml（子功能）。
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// version 由 build.ps1 的 ldflags 注入。
var version = "dev"

// defaultConfigPath 总配置入口（内部再展开 ai.yml 与 features/*.yml）。
const defaultConfigPath = "config/config.yml"

func main() {
	first := ""
	if len(os.Args) > 1 {
		first = os.Args[1]
	}
	var rest []string // 子命令自身旗标（第一个参数之后的全部参数）
	if len(os.Args) > 2 {
		rest = os.Args[2:]
	}

	switch {
	case first == "":
		serveWebCmd(nil)
	case first == "serve" || first == "web":
		serveWebCmd(rest)
	case first == "mq":
		runMQ(rest)
	case first == "cron":
		runCron(rest)
	case first == "version" || first == "-v" || first == "--version":
		fmt.Printf("harness %s\n", version)
	case strings.HasPrefix(first, "-"):
		// 直接给旗标（如 harness -addr :8080）沿用 Web 模式，保持零配置可用
		serveWebCmd(os.Args[1:])
	case isImagePath(first):
		runCLI()
	default:
		fmt.Fprintf(os.Stderr, "harness: 无法识别的参数 %q\n\n", first)
		usage()
		os.Exit(2)
	}
}

// usage 打印子命令清单（参数无法识别时输出到 stderr）。
func usage() {
	fmt.Fprint(os.Stderr, `用法: harness [子命令] [参数]

  (无参数)                 Web/API 服务，自动选端口并打开浏览器
  serve [-addr :8000]      同上；旗标 -addr / -config / -no-browser
  <图片路径> [总配置]       CLI 单图审读，打印最终报告
  mq [-config p]           消息队列消费者（骨架）
  cron [-interval 1h]      定时任务调度（骨架）
  version                  打印版本号
`)
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

// applyLogLevel 按 config.logging.level 设置 slog 默认级别（各运行模式共用）。
func applyLogLevel(level string) {
	switch strings.ToLower(level) {
	case "debug":
		slog.SetLogLoggerLevel(slog.LevelDebug)
	case "warn", "warning":
		slog.SetLogLoggerLevel(slog.LevelWarn)
	case "error":
		slog.SetLogLoggerLevel(slog.LevelError)
	default:
		slog.SetLogLoggerLevel(slog.LevelInfo)
	}
}

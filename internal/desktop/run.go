// Package desktop 承载 harness 可执行入口的运行模式分派：
//
//	harness                        Wails 桌面应用（默认：打开主窗口，内嵌前端）
//	harness [-config p]            同上，显式指定总配置路径
//	harness <图片路径> [总配置]     CLI 单图审读，打印最终报告
//	harness mq [-config p]         消息队列消费者（骨架）
//	harness cron [-interval 1h]    定时任务调度（骨架）
//	harness version                打印版本号
//
// 唯一的 main 包是仓库根的 main.go（Wails CLI 要求主包在模块根），委托给本包的 Run。
// 配置为三层结构：config/config.yml（总配置）→ ai.yml（AI provider）→ features/*.yml（子功能）。
package desktop

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// appVersion 由 main 包经 ldflags 注入后传入 Run（仅用于展示）。
var appVersion = "dev"

// defaultConfigPath 总配置入口（内部再展开 ai.yml 与 features/*.yml）。
const defaultConfigPath = "config/config.yml"

// Run 按第一个参数分派运行模式（args 为完整 os.Args）。
func Run(args []string, version string) {
	appVersion = version

	first := ""
	if len(args) > 1 {
		first = args[1]
	}
	rest := args[min(2, len(args)):]

	switch {
	case first == "" || first == "serve" || first == "web" || strings.HasPrefix(first, "-"):
		// 默认启动桌面应用；旗标（如 -config p）一并解析，保持零配置可用
		runDesktop(desktopConfigPath(args[1:]))
	case first == "mq":
		runMQ(rest)
	case first == "cron":
		runCron(rest)
	case first == "version" || first == "-v" || first == "--version":
		fmt.Printf("harness %s\n", appVersion)
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

  (无参数)                 Wails 桌面应用（内嵌前端，不再有 Web 服务）
  -config <路径>           指定总配置文件路径（默认 config/config.yml）
  <图片路径> [总配置]       CLI 单图审读，打印最终报告
  mq [-config p]           消息队列消费者（骨架）
  cron [-interval 1h]      定时任务调度（骨架）
  version                  打印版本号
`)
}

// desktopConfigPath 从旗标参数里挑出 -config 的值（其余旗标忽略，桌面模式零配置可用）。
func desktopConfigPath(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-config", "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
		case "-h", "-help", "--help":
			fmt.Print(`harness - LLM 工作流桌面应用

  (无参数)                 打开桌面窗口（内嵌前端）
  -config <路径>           指定总配置文件路径
  <图片路径> [总配置]       CLI 单图审读
  mq / cron / version      子命令（见 usage）
`)
			os.Exit(0)
		}
	}
	return defaultConfigPath
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

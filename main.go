// harness 是 AI 工作流平台的可执行入口（Wails CLI 从模块根构建主包，
// 故根目录 main.go 是唯一入口，委托 internal/desktop.Run）。
package main

import (
	"os"

	"go-ai-agent/internal/desktop"
)

// version 由 build.ps1 / wails build 的 ldflags 注入。
var version = "dev"

func main() {
	desktop.Run(os.Args, version)
}

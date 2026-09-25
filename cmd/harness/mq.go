package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"go-ai-agent/internal/config"
)

// runMQ 消息队列消费者入口（骨架）。
//
// 目前仅搭建进程生命周期：加载配置 → 初始化日志 → 监听退出信号。
// 真实的消费逻辑（连接 broker、订阅主题、调度工作流）留待后续实现。
func runMQ(args []string) {
	fs := flag.NewFlagSet("mq", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "总配置文件路径")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness mq:", err)
		os.Exit(1)
	}
	applyLogLevel(cfg.Logging.Level)

	slog.Info("mq consumer starting", "version", version)

	// TODO: 连接消息队列 broker，订阅主题，并将收到的任务分派给工作流处理。
	// 目前进程仅阻塞等待退出信号，保持骨架可编译、可运行。

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	slog.Info("mq consumer shutting down")
}

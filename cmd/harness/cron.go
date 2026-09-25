package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go-ai-agent/internal/config"
)

// runCron 定时任务调度入口（骨架）。
//
// 目前仅搭建进程生命周期：加载配置 → 初始化日志 → 启动一个占位 ticker → 监听退出信号。
// 真实的调度逻辑（cron 表达式、任务注册表、按周期触发工作流）留待后续实现。
func runCron(args []string) {
	fs := flag.NewFlagSet("cron", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "总配置文件路径")
	interval := fs.Duration("interval", time.Hour, "占位调度周期（骨架示例，真实调度待实现）")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness cron:", err)
		os.Exit(1)
	}
	applyLogLevel(cfg.Logging.Level)

	slog.Info("cron scheduler starting", "version", version, "interval", interval.String())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// TODO: 用真正的 cron 库（表达式解析 + 任务注册表）替换以下占位 ticker。
	// 目前仅周期性打印一条日志，保持骨架可编译、可运行。
	go func() {
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				slog.Info("cron tick (placeholder)", "at", t.Format(time.RFC3339))
			}
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	slog.Info("cron scheduler shutting down")
}

package main

import (
	"context"
	"fmt"
	"os"

	"go-ai-agent/internal/config"
	"go-ai-agent/internal/graph"
	"go-ai-agent/internal/graph/nodes"
	"go-ai-agent/internal/iolog"
	"go-ai-agent/internal/llm"
	"go-ai-agent/internal/stats"
)

// runCLI 单图审读：读配置 → 建 provider → 跑流水线 → 打印最终报告。
func runCLI() {
	if err := cliRun(); err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
}

func cliRun() error {
	imagePath := os.Args[1]
	cfgPath := "config/ai.yml"
	if len(os.Args) > 2 {
		cfgPath = os.Args[2]
	} else if v := os.Getenv("AI_CONFIG"); v != "" {
		cfgPath = v
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	// llm-stats：enabled=false 仅关闭落库（调用路径不变）
	var recorder *stats.Recorder
	var sink *llm.StatsSink
	if cfg.LLMStats.Enabled {
		recorder, err = stats.Open(cfg.LLMStats.DBPath, cfg.LLMStats.RetentionDays)
		if err != nil {
			return fmt.Errorf("init llm-stats: %w", err)
		}
		defer recorder.Close()
		sink = llm.NewStatsSink(recorder, cfg.LLMStats.QueueCapacity)
		defer sink.Close()
	}

	factory, err := llm.NewFactory(cfg.LLM.Providers, sink, iolog.New(cfg.LLM.IoLog).Hook())
	if err != nil {
		return fmt.Errorf("init llm factory: %w", err)
	}

	app, err := graph.Build(nodes.Deps{
		Factory: factory,
		Steps:   cfg.LLM.Steps,
		Default: config.Step{
			Provider:    cfg.LLM.DefaultProvider,
			Model:       envOr("HARNESS_DEFAULT_MODEL", "qwen3-vl-8b"),
			Temperature: cfg.LLM.Temperature,
			MaxTokens:   cfg.LLM.MaxTokens,
		},
		Stream: cfg.LLM.StreamOutput,
	})
	if err != nil {
		return fmt.Errorf("build graph: %w", err)
	}

	state := nodes.State{ImagePath: imagePath, RetryCount: map[string]int{}}
	final, err := app.Invoke(context.Background(), &state)
	if err != nil {
		return fmt.Errorf("invoke workflow: %w", err)
	}
	fs, ok := final.(*nodes.State)
	if !ok {
		return fmt.Errorf("unexpected final state type %T", final)
	}
	fmt.Println(fs.FinalReport)
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

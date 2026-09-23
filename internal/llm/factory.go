// Package llm 提供 provider → OpenAI 兼容客户端的工厂，以及带统计埋点的包装层。
package llm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"

	"go-ai-agent/internal/config"
)

// Model 统一的模型调用接口（便于 mock 与包装）。
type Model interface {
	GenerateContent(ctx context.Context, messages []llms.MessageContent, opts ...llms.CallOption) (*llms.ContentResponse, error)
}

// Factory 按 provider.name + model 缓存 LLM 客户端。
type Factory struct {
	mu        sync.RWMutex
	cache     map[string]Model
	providers map[string]config.Provider
	stats     *StatsSink // 可为 nil
	ioLog     *IoLogHook // 可为 nil
}

// NewFactory 创建工厂；stats/ioLog 可为 nil（未启用时）。
// 启动期预热所有 provider：缺哪个立即报错，避免运行时 panic 难定位。
func NewFactory(providers []config.Provider, stats *StatsSink, ioLog *IoLogHook) (*Factory, error) {
	m := make(map[string]config.Provider, len(providers))
	for _, p := range providers {
		m[p.Name] = p
	}
	f := &Factory{cache: map[string]Model{}, providers: m, stats: stats, ioLog: ioLog}
	for name, p := range m {
		if _, err := f.build(p, "probe-model"); err != nil {
			return nil, fmt.Errorf("预热 provider %q 失败: %w", name, err)
		}
	}
	return f, nil
}

// Get 返回指定 provider+model 的客户端（带统计埋点）。
func (f *Factory) Get(providerName, model string) (Model, error) {
	key := providerName + "|" + model
	f.mu.RLock()
	l, ok := f.cache[key]
	f.mu.RUnlock()
	if ok {
		return l, nil
	}
	p, ok := f.providers[providerName]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerName)
	}
	inner, err := f.build(p, model)
	if err != nil {
		return nil, err
	}
	wrapped := &instrumentedLLM{inner: inner, provider: providerName, model: model, factory: f}
	f.mu.Lock()
	f.cache[key] = wrapped
	f.mu.Unlock()
	return wrapped, nil
}

func (f *Factory) build(p config.Provider, model string) (*openai.LLM, error) {
	httpClient := &http.Client{
		Timeout: time.Duration(p.TimeoutSeconds) * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: time.Duration(p.ConnectTimeoutSeconds) * time.Second,
			}).DialContext,
		},
	}
	return openai.New(
		openai.WithBaseURL(p.BaseURL), // langchaingo 自动追加 /v1，yml 中不要带
		openai.WithToken(p.APIKey),
		openai.WithModel(model),
		openai.WithHTTPClient(httpClient),
	)
}

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
	wireDump  bool       // HTTP 层抓包（io-log.debug 开启时置位）

	caps     *Cache // 结果缓存（未启用时为 nil，所有操作 no-op）
	sems     map[string]chan struct{}
	semLimit map[string]int
	semMu    sync.Mutex
}

// SetCache 注入 LLM 结果缓存（相同输入命中即不再请求端点）。传 nil 表示关闭。
func (f *Factory) SetCache(c *Cache) { f.caps = c }

// SetProviderConcurrency 设定每个 provider 的并发请求上限（<=0 归一为 1）。
// 需在首次 Acquire 之前调用（本项目在服务启动时设置），本地推理端点
// 被并发打爆时起到闸门作用。
func (f *Factory) SetProviderConcurrency(limit int) {
	f.semMu.Lock()
	defer f.semMu.Unlock()
	if limit <= 0 {
		limit = 1
	}
	// 记录默认上限，各 provider 的信号量在首次 Acquire 时按此值创建
	f.semLimit[""] = limit
}

// Acquire 获取指定 provider 的一个并发额度，返回释放函数。
// 额度占满时阻塞等待，ctx 取消（如客户端断开/超时）则立即返回错误。
func (f *Factory) Acquire(ctx context.Context, provider string) (func(), error) {
	f.semMu.Lock()
	sem, ok := f.sems[provider]
	if !ok {
		limit := f.semLimit[""]
		if limit <= 0 {
			limit = 1
		}
		sem = make(chan struct{}, limit)
		f.sems[provider] = sem
	}
	f.semMu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	default:
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// EnableWireDump 开启 HTTP 层请求/响应原文日志（排查端点交互问题时使用）。
func (f *Factory) EnableWireDump() { f.wireDump = true }

// NewFactory 创建工厂；stats/ioLog 可为 nil（未启用时）。
// 启动期预热所有 provider：缺哪个立即报错，避免运行时 panic 难定位。
func NewFactory(providers []config.Provider, stats *StatsSink, ioLog *IoLogHook) (*Factory, error) {
	m := make(map[string]config.Provider, len(providers))
	for _, p := range providers {
		m[p.Name] = p
	}
	f := &Factory{cache: map[string]Model{}, providers: m, stats: stats, ioLog: ioLog,
		sems: map[string]chan struct{}{}, semLimit: map[string]int{}}
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
	transport := http.RoundTripper(&http.Transport{
		DialContext: (&net.Dialer{
			Timeout: time.Duration(p.ConnectTimeoutSeconds) * time.Second,
		}).DialContext,
	})
	if f.wireDump {
		transport = &wireDumpTransport{next: transport}
	}
	httpClient := &http.Client{
		Timeout:   time.Duration(p.TimeoutSeconds) * time.Second,
		Transport: transport,
	}
	return openai.New(
		openai.WithBaseURL(p.BaseURL), // 注意：langchaingo 仅做 baseURL+"/chat/completions" 拼接，不会补 /v1，yml 中 base-url 须自带 /v1
		openai.WithToken(p.APIKey),
		openai.WithModel(model),
		openai.WithHTTPClient(httpClient),
	)
}

// Provider 返回 provider 原始配置（含 api-key，仅限服务端内部使用）。
func (f *Factory) Provider(name string) (config.Provider, bool) {
	p, ok := f.providers[name]
	return p, ok
}

// HTTPClient 按 provider 的超时配置构造 HTTP 客户端（直连 OpenAI 兼容端点时使用）。
func (f *Factory) HTTPClient(p config.Provider) *http.Client {
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: time.Duration(p.ConnectTimeoutSeconds) * time.Second,
			}).DialContext,
		},
	}
}

// LogChat 对话直连路径的 io-log 埋点：复用注入的钩子记录输入与输出（同一 request_id 配对）。
func (f *Factory) LogChat(provider, model string, messages []llms.MessageContent, answer, reasoning string, err error, latencyMs int64) {
	h := f.ioLog
	if h == nil {
		return
	}
	id := ""
	if h.NewID != nil {
		id = h.NewID()
	}
	if h.OnRequest != nil {
		h.OnRequest(id, provider, model, messages)
	}
	if h.OnResponse == nil {
		return
	}
	resp := &llms.ContentResponse{}
	if err == nil {
		resp.Choices = []*llms.ContentChoice{{Content: answer, ReasoningContent: reasoning}}
	}
	h.OnResponse(id, provider, model, resp, err, int(latencyMs))
}

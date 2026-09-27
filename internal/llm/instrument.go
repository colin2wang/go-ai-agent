package llm

import (
	"context"
	"strings"
	"time"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

// StatsLogger 由 stats.Recorder 实现，异步队列只依赖此接口，避免包循环。
type StatsLogger interface {
	Log(provider, model string, promptTokens, completionTokens, latencyMs int, ok bool, errMsg string)
}

// StatsSink 异步统计队列：调用方非阻塞入队，后台 goroutine 落库。
type StatsSink struct {
	ch   chan statEntry
	done chan struct{}
}

type statEntry struct {
	provider, model                           string
	promptTokens, completionTokens, latencyMs int
	ok                                        bool
	errMsg                                    string
}

// NewStatsSink 创建容量为 queueCapacity 的异步落库队列并启动后台消费。
func NewStatsSink(logger StatsLogger, queueCapacity int) *StatsSink {
	if queueCapacity <= 0 {
		queueCapacity = 10000
	}
	s := &StatsSink{ch: make(chan statEntry, queueCapacity), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for e := range s.ch {
			logger.Log(e.provider, e.model, e.promptTokens, e.completionTokens, e.latencyMs, e.ok, e.errMsg)
		}
	}()
	return s
}

// Close 停止接收并等待队列排空。
func (s *StatsSink) Close() {
	if s == nil {
		return
	}
	close(s.ch)
	<-s.done
}

func (s *StatsSink) record(e statEntry) {
	if s == nil {
		return
	}
	select {
	case s.ch <- e:
	default: // 队列满则丢弃，绝不阻塞调用链路
	}
}

// RecordChat 对话直连路径的统计埋点：异步上报一次调用（队列满即丢弃）。
func (f *Factory) RecordChat(provider, model string, promptTokens, completionTokens int, latencyMs int64, err error) {
	f.stats.record(statEntry{
		provider: provider, model: model,
		promptTokens: promptTokens, completionTokens: completionTokens,
		latencyMs: int(latencyMs), ok: err == nil, errMsg: errString(err),
	})
}

// IoLogHook 输入/输出日志钩子（由 iolog 包注入实现，避免包循环）。
// requestID 由调用方经 NewID 生成一次，同一次调用的输入/输出日志透传共用，便于配对检索。
type IoLogHook struct {
	NewID      func() string
	OnRequest  func(requestID, provider, model string, messages []llms.MessageContent)
	OnResponse func(requestID, provider, model string, resp *llms.ContentResponse, err error, latencyMs int)
}

// instrumentedLLM 在真实 LLM 外包一层：io-log + 耗时/Token/错误统计。
type instrumentedLLM struct {
	inner    *openai.LLM
	provider string
	model    string
	factory  *Factory
}

func (w *instrumentedLLM) GenerateContent(ctx context.Context, messages []llms.MessageContent, opts ...llms.CallOption) (*llms.ContentResponse, error) {
	// 结果缓存：命中则直接回放（不发请求、不计入调用流水），并记一条 io-log 便于排障。
	if w.factory.caps.decision(ctx, false) {
		if key := CacheKey(w.provider, w.model, messages, cacheParamsFrom(opts)); key != "" {
			if _, _, answer, reasoning, pt, ct, ok := w.factory.caps.store.CacheGet(key); ok {
				resp := cachedResponse(w.model, answer, reasoning, pt, ct)
				w.logCacheHit(messages, resp)
				return resp, nil
			}
		}
	}

	id := ""
	if h := w.factory.ioLog; h != nil && h.OnRequest != nil {
		if h.NewID != nil {
			id = h.NewID()
		}
		h.OnRequest(id, w.provider, w.model, messages)
	}
	start := time.Now()
	resp, err := w.inner.GenerateContent(ctx, messages, opts...)
	latency := time.Since(start).Milliseconds()
	if h := w.factory.ioLog; h != nil && h.OnResponse != nil {
		h.OnResponse(id, w.provider, w.model, resp, err, int(latency))
	}
	pt, ct := usageTokens(resp)
	w.factory.stats.record(statEntry{
		provider: w.provider, model: w.model,
		promptTokens: pt, completionTokens: ct, latencyMs: int(latency),
		ok: err == nil, errMsg: errString(err),
	})
	if err == nil && w.factory.caps.decision(ctx, false) {
		w.factory.caps.put(w.provider, w.model, messages, opts, resp, pt, ct)
	}
	return resp, err
}

// logCacheHit 为命中缓存的调用补一条 io-log（用独立 request_id，不与真实请求混淆）。
func (w *instrumentedLLM) logCacheHit(messages []llms.MessageContent, resp *llms.ContentResponse) {
	h := w.factory.ioLog
	if h == nil || h.OnResponse == nil {
		return
	}
	id := ""
	if h.NewID != nil {
		id = h.NewID()
	}
	if h.OnRequest != nil {
		h.OnRequest(id, w.provider, w.model, messages)
	}
	h.OnResponse(id, w.provider, w.model, resp, nil, 0)
}

// cachedResponse 由缓存行重建一个与端点响应等价的 ContentResponse。
func cachedResponse(model, answer, reasoning string, pt, ct int) *llms.ContentResponse {
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{
		Content:          answer,
		ReasoningContent: reasoning,
		GenerationInfo: map[string]any{
			"PromptTokens":     pt,
			"CompletionTokens": ct,
			"ModelName":        model,
			"FromCache":        true,
		},
	}}}
}

// cacheParamsFrom 从 CallOption 列表中提取参与缓存哈希的参数。
// langchaingo 未导出读取接口，这里把选项施加到零值配置上再取所需字段：
// 无需感知具体选项实现，未显式设置的字段保持零值，不同调用之间仍可稳定比对。
func cacheParamsFrom(opts []llms.CallOption) cacheParams {
	cur := &llms.CallOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(cur)
		}
	}
	return cacheParams{
		Temperature: cur.Temperature,
		MaxTokens:   cur.MaxTokens,
		StopWords:   append([]string(nil), cur.StopWords...),
		JSONMode:    cur.JSONMode,
		TopK:        cur.TopK,
		TopP:        cur.TopP,
		Seed:        cur.Seed,
		N:           cur.N,
	}
}

// put 写入一条缓存（失败不影响主链路：只是失去一次复用机会）。
func (c *Cache) put(provider, model string,
	messages []llms.MessageContent, opts []llms.CallOption, resp *llms.ContentResponse,
	promptTokens, completionTokens int) {

	if !c.Enabled() || resp == nil || len(resp.Choices) == 0 {
		return
	}
	key := CacheKey(provider, model, messages, cacheParamsFrom(opts))
	if key == "" {
		return
	}
	ch := resp.Choices[0]
	if strings.TrimSpace(ch.Content) == "" && !c.cfg.KeepOnErr {
		return // 空回答不写缓存：下次仍应真实重试
	}
	var exp time.Time
	if ttl := c.TTL(); ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	_ = c.store.CachePut(key, provider, model, ch.Content, ch.ReasoningContent,
		promptTokens, completionTokens, exp)
}

// usageTokens 尽力从 GenerationInfo 中提取 prompt/completion token 数
// （langchaingo 的 ContentChoice 无直接 Usage 字段，token 数存于 GenerationInfo）。
func usageTokens(resp *llms.ContentResponse) (prompt, completion int) {
	if resp == nil || len(resp.Choices) == 0 {
		return 0, 0
	}
	info := resp.Choices[0].GenerationInfo
	if info == nil {
		return 0, 0
	}
	getInt := func(keys ...string) int {
		for _, k := range keys {
			if v, ok := info[k]; ok {
				switch n := v.(type) {
				case int:
					return n
				case int64:
					return int(n)
				case float64:
					return int(n)
				}
			}
		}
		return 0
	}
	return getInt("PromptTokens", "prompt_tokens"),
		getInt("CompletionTokens", "completion_tokens")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

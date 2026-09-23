package llm

import (
	"context"
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

// IoLogHook 输入/输出日志钩子（由 iolog 包注入实现，避免包循环）。
type IoLogHook struct {
	OnRequest  func(provider, model string, messages []llms.MessageContent)
	OnResponse func(provider, model string, resp *llms.ContentResponse, err error, latencyMs int)
}

// instrumentedLLM 在真实 LLM 外包一层：io-log + 耗时/Token/错误统计。
type instrumentedLLM struct {
	inner    *openai.LLM
	provider string
	model    string
	factory  *Factory
}

func (w *instrumentedLLM) GenerateContent(ctx context.Context, messages []llms.MessageContent, opts ...llms.CallOption) (*llms.ContentResponse, error) {
	if h := w.factory.ioLog; h != nil && h.OnRequest != nil {
		h.OnRequest(w.provider, w.model, messages)
	}
	start := time.Now()
	resp, err := w.inner.GenerateContent(ctx, messages, opts...)
	latency := time.Since(start).Milliseconds()
	if h := w.factory.ioLog; h != nil && h.OnResponse != nil {
		h.OnResponse(w.provider, w.model, resp, err, int(latency))
	}
	pt, ct := usageTokens(resp)
	w.factory.stats.record(statEntry{
		provider: w.provider, model: w.model,
		promptTokens: pt, completionTokens: ct, latencyMs: int(latency),
		ok: err == nil, errMsg: errString(err),
	})
	return resp, err
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

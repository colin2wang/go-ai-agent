package llm

import (
	"context"
	"strings"
	"sync"

	"github.com/tmc/langchaingo/llms"
)

// StreamAccumulator 流式输出累积器：REASONING + ANSWER 增量块分别累积，
// 通过 llms.WithStreamingFunc(acc.Callback) 接入。
type StreamAccumulator struct {
	mu        sync.Mutex
	Answer    strings.Builder
	Reasoning strings.Builder
}

// Callback 作为 llms.WithStreamingFunc 的回调，按 chunk 类型分流累积。
func (a *StreamAccumulator) Callback(ctx context.Context, chunk []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Answer.Write(chunk)
	return nil
}

// AnswerText 返回累积的回答全文。
func (a *StreamAccumulator) AnswerText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Answer.String()
}

// ReasoningText 返回累积的推理全文。
func (a *StreamAccumulator) ReasoningText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Reasoning.String()
}

// StreamOption 调用选项组装：按配置生成流式/非流式的 CallOption 列表。
// stream=true 时统一流式接收并经 acc 累积；false 时回退一次返回的普通调用。
func StreamOptions(stream bool, acc *StreamAccumulator, temperature float64, maxTokens int) []llms.CallOption {
	opts := []llms.CallOption{
		llms.WithTemperature(temperature),
		llms.WithMaxTokens(maxTokens),
	}
	if stream && acc != nil {
		opts = append(opts, llms.WithStreamingFunc(acc.Callback))
	}
	return opts
}

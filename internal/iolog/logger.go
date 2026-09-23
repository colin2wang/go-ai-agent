// Package iolog 提供按 head/tail-chars 截断的输入输出日志。
package iolog

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/tmc/langchaingo/llms"

	"go-ai-agent/internal/config"
	"go-ai-agent/internal/llm"
)

// Logger 每次调用生成 UUID 关联输入与输出。
type Logger struct {
	enabled   bool
	headChars int
	tailChars int
}

// New 根据配置创建；enabled=false 时所有方法为空操作。
func New(cfg config.IoLog) *Logger {
	return &Logger{enabled: cfg.Enabled, headChars: cfg.HeadChars, tailChars: cfg.TailChars}
}

// Hook 返回注入 llm.Factory 的钩子（始终非 nil，禁用时内部短路）。
func (l *Logger) Hook() *llm.IoLogHook {
	hook := &llm.IoLogHook{
		OnRequest: l.LogRequest,
		OnResponse: func(provider, model string, resp *llms.ContentResponse, err error, latencyMs int) {
			l.LogResponse(provider, model, resp, err, int64(latencyMs))
		},
	}
	return hook
}

// Truncate 按 head/tail 截断，中间标注省略字符数；不足 head+tail+20 直接返回原文。
func Truncate(s string, head, tail int) string {
	r := []rune(s)
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	if len(r) <= head+tail+20 {
		return s
	}
	return string(r[:head]) + "...[省略 " + strconv.Itoa(len(r)-head-tail) + " 字符]..." + string(r[len(r)-tail:])
}

// LogRequest 记录一次调用的输入：system/user 截断 + 总字数；多模态只记文件名与媒体类型。
func (l *Logger) LogRequest(provider, model string, messages []llms.MessageContent) {
	if l == nil || !l.enabled {
		return
	}
	id := newUUID()
	var b strings.Builder
	totalChars := 0
	for i, m := range messages {
		role := string(m.Role)
		for _, part := range m.Parts {
			switch p := part.(type) {
			case llms.TextContent:
				s := p.Text
				totalChars += len([]rune(s))
				fmt.Fprintf(&b, "\n[msg %d %s] %s", i, role, Truncate(s, l.headChars, l.tailChars))
			case llms.BinaryContent:
				totalChars += len(p.Data)
				// 多模态输入只记录文件名与媒体类型，不记录二进制内容
				fmt.Fprintf(&b, "\n[msg %d %s] <binary mime=%s bytes=%d>", i, role, p.MIMEType, len(p.Data))
			}
		}
	}
	slog.Info("llm io-log request",
		slog.String("request_id", id),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.Int("total_chars", totalChars),
		slog.String("input", b.String()),
	)
}

// LogResponse 记录一次调用的输出：回答全文；thinking 前后截断 + 总字数。
func (l *Logger) LogResponse(provider, model string, resp *llms.ContentResponse, err error, latencyMs int64) {
	if l == nil || !l.enabled {
		return
	}
	id := newUUID()
	if err != nil {
		slog.Error("llm io-log response",
			slog.String("request_id", id),
			slog.String("provider", provider),
			slog.String("model", model),
			slog.Int64("latency_ms", latencyMs),
			slog.String("error", err.Error()),
		)
		return
	}
	answer := ""
	if len(resp.Choices) > 0 {
		answer = resp.Choices[0].Content
	}
	reasoning := extractReasoning(resp)
	slog.Info("llm io-log response",
		slog.String("request_id", id),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.Int64("latency_ms", latencyMs),
		slog.Int("answer_chars", len([]rune(answer))),
		slog.String("answer", answer),
		slog.Int("reasoning_chars", len([]rune(reasoning))),
		slog.String("reasoning", Truncate(reasoning, l.headChars, l.tailChars)),
	)
}

// extractReasoning 兼容 langchaingo 不同字段名，尽力提取思考链文本。
func extractReasoning(resp *llms.ContentResponse) string {
	if len(resp.Choices) == 0 {
		return ""
	}
	c := resp.Choices[0]
	if c.ReasoningContent != "" {
		return c.ReasoningContent
	}
	return ""
}

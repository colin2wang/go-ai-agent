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
// request_id 由调用方经 NewID 生成一次，同一次调用的输入/输出日志共用，便于配对检索。
func (l *Logger) Hook() *llm.IoLogHook {
	return &llm.IoLogHook{
		NewID: newUUID,
		OnRequest: func(requestID, provider, model string, messages []llms.MessageContent) {
			l.logRequest(requestID, provider, model, messages)
		},
		OnResponse: func(requestID, provider, model string, resp *llms.ContentResponse, err error, latencyMs int) {
			l.logResponse(requestID, provider, model, resp, err, int64(latencyMs))
		},
	}
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

// oneline 将多行文本折叠为一行（换行/制表/连续空格 → 单空格），保持日志行整洁可扫描。
func oneline(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// recentMsgCount 多轮对话日志中详细输出的末尾消息数（更早的历史只计数）。
const recentMsgCount = 2

// logRequest 记录一次调用的输入：
// 短输入（工作流，通常 system+user）逐条输出；多轮对话仅输出首条（system）与末尾
// recentMsgCount 条，中间历史以 history_msgs 计数代替，避免每轮重复刷屏。
// 文本统一折叠为单行并按 head/tail 截断；总字数始终按全量统计。
// 多模态输入只记录媒体类型与字节数，不记录二进制内容。
func (l *Logger) logRequest(requestID, provider, model string, messages []llms.MessageContent) {
	if l == nil || !l.enabled {
		return
	}
	if requestID == "" {
		requestID = newUUID()
	}
	totalChars := 0
	for _, m := range messages {
		for _, part := range m.Parts {
			switch p := part.(type) {
			case llms.TextContent:
				totalChars += len([]rune(p.Text))
			case llms.BinaryContent:
				totalChars += len(p.Data)
			}
		}
	}

	// 详细输出选择：首条 + 末尾 recentMsgCount 条；消息不多时全部输出
	detail := make(map[int]bool, recentMsgCount+1)
	history := 0
	for i := range messages {
		if i == 0 || i >= len(messages)-recentMsgCount {
			detail[i] = true
		} else {
			history++
		}
	}

	const sep = "  |  "
	var b strings.Builder
	for i, m := range messages {
		if !detail[i] {
			continue
		}
		role := string(m.Role)
		for _, part := range m.Parts {
			switch p := part.(type) {
			case llms.TextContent:
				fmt.Fprintf(&b, "[msg %d %s] %s%s", i, role, oneline(Truncate(p.Text, l.headChars, l.tailChars)), sep)
			case llms.BinaryContent:
				fmt.Fprintf(&b, "[msg %d %s] <binary mime=%s bytes=%d>%s", i, role, p.MIMEType, len(p.Data), sep)
			}
		}
	}
	attrs := []any{
		slog.String("request_id", requestID),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.Int("total_chars", totalChars),
	}
	if history > 0 {
		attrs = append(attrs, slog.Int("history_msgs", history))
	}
	attrs = append(attrs, slog.String("input", strings.TrimSuffix(b.String(), sep)))
	slog.Info("llm io-log request", attrs...)
}

// logResponse 记录一次调用的输出：answer 与 reasoning 均截断 + 折叠为单行；
// reasoning 为空时省略该字段。
func (l *Logger) logResponse(requestID, provider, model string, resp *llms.ContentResponse, err error, latencyMs int64) {
	if l == nil || !l.enabled {
		return
	}
	if requestID == "" {
		requestID = newUUID()
	}
	if err != nil {
		slog.Error("llm io-log response",
			slog.String("request_id", requestID),
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
	attrs := []any{
		slog.String("request_id", requestID),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.Int64("latency_ms", latencyMs),
		slog.Int("answer_chars", len([]rune(answer))),
		slog.String("answer", oneline(Truncate(answer, l.headChars, l.tailChars))),
	}
	if reasoning != "" {
		attrs = append(attrs,
			slog.Int("reasoning_chars", len([]rune(reasoning))),
			slog.String("reasoning", oneline(Truncate(reasoning, l.headChars, l.tailChars))),
		)
	}
	slog.Info("llm io-log response", attrs...)
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

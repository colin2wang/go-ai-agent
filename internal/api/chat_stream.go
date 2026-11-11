package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go-ai-agent/internal/config"
)

// chatSink chat 流式事件的接收端。桌面端实现把事件转发为 Wails 运行时事件。
// 事件与旧版 SSE 协议保持一致：meta / delta / done / error，另加 collect
// 承载非流式一次性结果（端点不支持流式自动降级时使用）。
type chatSink interface {
	Emit(event string, payload any)
}

// streamChatCore 流式转发：上游 SSE → sink 事件。
// 端点拒绝 stream=true 或未按 SSE 返回时自动降级，以 collect 事件下发完整结果。
func (s *Server) streamChatCore(ctx context.Context, sink chatSink, p config.Provider, cp *chatPrepared) error {
	payload := chatUpstream{Model: cp.model, Messages: wireMessages(cp.messages), Temperature: cp.temperature, MaxTokens: cp.maxTokens, Stream: true, ReasoningEffort: cp.reasoningEffort}
	client := s.factory.HTTPClient(p)
	start := time.Now()
	resp, err := doProviderRequest(ctx, client, p, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return fmt.Errorf("请求 LLM 失败: %w", err)
	}
	// 调用时检测：端点拒绝 stream=true（非 200）→ 自动降级为非流式重试一次
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		note := fmt.Sprintf("流式请求被端点拒绝（HTTP %d：%s），本次已自动改用非流式返回", resp.StatusCode, snippet(body))
		slog.Debug("chat stream rejected, falling back", slog.String("provider", p.Name), slog.String("model", cp.model))
		out, cerr := s.collectChatCore(ctx, p, cp, note)
		if cerr != nil {
			return cerr
		}
		sink.Emit("collect", out)
		return nil
	}
	// 200 但不是 SSE（端点忽略 stream=true 按普通 JSON 回）→ 同样按非流式一次性下发
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return fmt.Errorf("读取响应失败: %w", err)
		}
		var out chatCompletionResponse
		if json.Unmarshal(body, &out) != nil {
			return fmt.Errorf("解析响应失败: %w", err)
		}
		sink.Emit("collect", s.chatCollectResult(p, cp, &out, time.Since(start),
			"端点未以 SSE 返回（忽略了 stream=true），本次已按非流式处理"))
		return nil
	}
	defer resp.Body.Close()

	sink.Emit("meta", map[string]any{"provider": p.Name, "model": cp.model})

	var answer, reasoning strings.Builder
	usage := &chatUsage{}
	var streamErr error
	br := bufio.NewReader(resp.Body)
	for {
		if ctx.Err() != nil {
			break // 调用方取消
		}
		line, err := br.ReadString('\n')
		if err != nil {
			// 非正常结束才报错；EOF/连接关闭视为上游完成
			if err != io.EOF && ctx.Err() == nil {
				streamErr = err
			}
			break
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payloadStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payloadStr == "[DONE]" {
			break
		}
		var chunk chatStreamChunk
		if json.Unmarshal([]byte(payloadStr), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			*usage = *chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				answer.WriteString(c.Delta.Content)
				sink.Emit("delta", map[string]any{"type": "answer", "text": c.Delta.Content})
			}
			if c.Delta.ReasoningContent != "" {
				reasoning.WriteString(c.Delta.ReasoningContent)
				sink.Emit("delta", map[string]any{"type": "reasoning", "text": c.Delta.ReasoningContent})
			}
		}
	}
	latency := time.Since(start)
	// 上游未回传 usage 时按字符数粗估（仅用于统计展示，不追求精确）
	if usage.PromptTokens == 0 {
		usage.PromptTokens = estimatePromptTokens(cp.messages)
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens = estimateTokens(answer.String() + reasoning.String())
	}
	s.factory.RecordChat(p.Name, cp.model, usage.PromptTokens, usage.CompletionTokens, latency.Milliseconds(), streamErr)
	s.factory.LogChat(p.Name, cp.model, toMessageContents(cp.messages), answer.String(), reasoning.String(), streamErr, latency.Milliseconds())

	if streamErr != nil {
		sink.Emit("error", map[string]any{"error": "流式中断: " + streamErr.Error()})
		return nil
	}
	sink.Emit("done", map[string]any{
		"duration_ms":       latency.Milliseconds(),
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
	})
	return nil
}

// collectChatCore 非流式请求：一次性 chat/completions，返回完整结果（含降级说明）。
func (s *Server) collectChatCore(ctx context.Context, p config.Provider, cp *chatPrepared, note string) (map[string]any, error) {
	payload := chatUpstream{Model: cp.model, Messages: wireMessages(cp.messages), Temperature: cp.temperature, MaxTokens: cp.maxTokens, ReasoningEffort: cp.reasoningEffort}
	client := s.factory.HTTPClient(p)
	start := time.Now()
	resp, err := doProviderRequest(ctx, client, p, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return nil, fmt.Errorf("请求 LLM 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM 返回 HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var out chatCompletionResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return s.chatCollectResult(p, cp, &out, time.Since(start), note), nil
}

// chatCollectResult 组装一次性对话结果并补记统计与 io-log。
// note 非空时携带 stream_fallback 标记，前端据此提示并自动关闭流式开关。
func (s *Server) chatCollectResult(p config.Provider, cp *chatPrepared, out *chatCompletionResponse, latency time.Duration, note string) map[string]any {
	content, reason := "", ""
	if len(out.Choices) > 0 {
		content = out.Choices[0].Message.Content
		reason = out.Choices[0].Message.ReasoningContent
	}
	usage := out.Usage
	if usage == nil {
		usage = &chatUsage{
			PromptTokens:     estimatePromptTokens(cp.messages),
			CompletionTokens: estimateTokens(content + reason),
		}
	}
	s.factory.RecordChat(p.Name, cp.model, usage.PromptTokens, usage.CompletionTokens, latency.Milliseconds(), nil)
	s.factory.LogChat(p.Name, cp.model, toMessageContents(cp.messages), content, reason, nil, latency.Milliseconds())

	respBody := map[string]any{
		"provider":          p.Name,
		"model":             cp.model,
		"answer":            content,
		"reasoning":         reason,
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"duration_ms":       latency.Milliseconds(),
	}
	if note != "" {
		respBody["stream_fallback"] = true
		respBody["note"] = note
	}
	return respBody
}

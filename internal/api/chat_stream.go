package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go-ai-agent/internal/config"
)

// streamChat 流式转发：上游 SSE → 浏览器 SSE（事件 meta/delta/done/error）。
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, p config.Provider, model string,
	messages []chatMessage, temperature float64, maxTokens int, reasoningEffort string) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "当前服务不支持流式响应")
		return
	}
	payload := chatUpstream{Model: model, Messages: wireMessages(messages), Temperature: temperature, MaxTokens: maxTokens, Stream: true, ReasoningEffort: reasoningEffort}
	client := s.factory.HTTPClient(p)
	start := time.Now()
	resp, err := doProviderRequest(r.Context(), client, p, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		fail(w, http.StatusBadGateway, "请求 LLM 失败: "+err.Error())
		return
	}
	// 调用时检测：端点拒绝 stream=true（非 200）→ 自动降级为非流式重试一次
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		note := fmt.Sprintf("流式请求被端点拒绝（HTTP %d：%s），本次已自动改用非流式返回", resp.StatusCode, snippet(body))
		slog.Debug("chat stream rejected, falling back", slog.String("provider", p.Name), slog.String("model", model))
		s.collectChatWithNote(w, r, p, model, messages, temperature, maxTokens, reasoningEffort, note)
		return
	}
	// 200 但不是 SSE（端点忽略 stream=true 按普通 JSON 回）→ 同样按非流式一次性下发
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			fail(w, http.StatusBadGateway, "读取响应失败: "+err.Error())
			return
		}
		var out chatCompletionResponse
		if json.Unmarshal(body, &out) != nil {
			fail(w, http.StatusBadGateway, "解析响应失败: "+err.Error())
			return
		}
		s.respondChatJSON(w, p, model, messages, &out, time.Since(start),
			"端点未以 SSE 返回（忽略了 stream=true），本次已按非流式处理")
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	writeSSE(w, "meta", map[string]any{"provider": p.Name, "model": model})
	flusher.Flush()

	var answer, reasoning strings.Builder
	usage := &chatUsage{}
	var streamErr error
	br := bufio.NewReader(resp.Body)
	for {
		if r.Context().Err() != nil {
			break // 浏览器断开
		}
		line, err := br.ReadString('\n')
		if err != nil {
			// 非正常结束才报错；EOF/连接关闭视为上游完成
			if err != io.EOF && r.Context().Err() == nil {
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
				writeSSE(w, "delta", map[string]any{"type": "answer", "text": c.Delta.Content})
				flusher.Flush()
			}
			if c.Delta.ReasoningContent != "" {
				reasoning.WriteString(c.Delta.ReasoningContent)
				writeSSE(w, "delta", map[string]any{"type": "reasoning", "text": c.Delta.ReasoningContent})
				flusher.Flush()
			}
		}
	}
	latency := time.Since(start)
	// 上游未回传 usage 时按字符数粗估（仅用于统计展示，不追求精确）
	if usage.PromptTokens == 0 {
		usage.PromptTokens = estimatePromptTokens(messages)
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens = estimateTokens(answer.String() + reasoning.String())
	}
	s.factory.RecordChat(p.Name, model, usage.PromptTokens, usage.CompletionTokens, latency.Milliseconds(), streamErr)
	s.factory.LogChat(p.Name, model, toMessageContents(messages), answer.String(), reasoning.String(), streamErr, latency.Milliseconds())

	if streamErr != nil {
		writeSSE(w, "error", apiError{Error: "流式中断: " + streamErr.Error()})
		flusher.Flush()
		return
	}
	writeSSE(w, "done", map[string]any{
		"duration_ms":       latency.Milliseconds(),
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
	})
	flusher.Flush()
}

// collectChat 非流式：普通 chat/completions 一次性 JSON 返回。
func (s *Server) collectChat(w http.ResponseWriter, r *http.Request, p config.Provider, model string,
	messages []chatMessage, temperature float64, maxTokens int, reasoningEffort string) {

	s.collectChatWithNote(w, r, p, model, messages, temperature, maxTokens, reasoningEffort, "")
}

// collectChatWithNote 非流式请求；note 非空时随响应附带降级说明（流式回退路径复用）。
func (s *Server) collectChatWithNote(w http.ResponseWriter, r *http.Request, p config.Provider, model string,
	messages []chatMessage, temperature float64, maxTokens int, reasoningEffort, note string) {

	payload := chatUpstream{Model: model, Messages: wireMessages(messages), Temperature: temperature, MaxTokens: maxTokens, ReasoningEffort: reasoningEffort}
	client := s.factory.HTTPClient(p)
	start := time.Now()
	resp, err := doProviderRequest(r.Context(), client, p, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		fail(w, http.StatusBadGateway, "请求 LLM 失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		fail(w, http.StatusBadGateway, "读取响应失败: "+err.Error())
		return
	}
	if resp.StatusCode != http.StatusOK {
		fail(w, http.StatusBadGateway, fmt.Sprintf("LLM 返回 HTTP %d: %s", resp.StatusCode, snippet(body)))
		return
	}
	var out chatCompletionResponse
	if err := json.Unmarshal(body, &out); err != nil {
		fail(w, http.StatusBadGateway, "解析响应失败: "+err.Error())
		return
	}
	s.respondChatJSON(w, p, model, messages, &out, time.Since(start), note)
}

// respondChatJSON 一次性 JSON 下发（非流式或流式降级共用），并补记统计与 io-log。
// note 非空时携带 stream_fallback 标记，前端据此提示并自动关闭流式开关。
func (s *Server) respondChatJSON(w http.ResponseWriter, p config.Provider, model string,
	messages []chatMessage, out *chatCompletionResponse, latency time.Duration, note string) {

	content, reason := "", ""
	if len(out.Choices) > 0 {
		content = out.Choices[0].Message.Content
		reason = out.Choices[0].Message.ReasoningContent
	}
	usage := out.Usage
	if usage == nil {
		usage = &chatUsage{
			PromptTokens:     estimatePromptTokens(messages),
			CompletionTokens: estimateTokens(content + reason),
		}
	}
	s.factory.RecordChat(p.Name, model, usage.PromptTokens, usage.CompletionTokens, latency.Milliseconds(), nil)
	s.factory.LogChat(p.Name, model, toMessageContents(messages), content, reason, nil, latency.Milliseconds())

	respBody := map[string]any{
		"provider":          p.Name,
		"model":             model,
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
	respond(w, http.StatusOK, respBody)
}

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go-ai-agent/internal/config"
)

// chatImage 一条消息附带的图片（视觉模型输入）。
// Data 为纯 base64（不含 "data:image/png;base64," 前缀），前缀由服务端拼装。
type chatImage struct {
	Name string `json:"name,omitempty"`
	Mime string `json:"mime"`
	Data string `json:"data"`
}

// chatMessage 一条对话消息（role: system | user | assistant）。
// Images 仅 user 消息可用（纯文本请求体保持原样，不干扰原有调用方）。
type chatMessage struct {
	Role    string      `json:"role"`
	Content string      `json:"content"`
	Images  []chatImage `json:"images,omitempty"`
}

// chatSendRequest POST /api/chat/send 请求体。
type chatSendRequest struct {
	FeatureID   string        `json:"feature_id"`
	Provider    string        `json:"provider"`
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      *bool         `json:"stream"`      // nil = 跟随子功能/全局默认
	Temperature *float64      `json:"temperature"` // nil = 步骤配置
	MaxTokens   int           `json:"max_tokens"`  // nil/0 = 步骤配置
}

// chatImageURL / chatPart —— OpenAI 多模态 content 片段（type=text | image_url）。
type chatImageURL struct {
	URL string `json:"url"`
}

type chatPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

// chatWireMessage 实际发往 provider 的消息：纯文本时 content 仍为字符串，
// 带图片时才是片段数组——部分本地端点只接受字符串 form，不做无谓的数组化。
type chatWireMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// chatUsage OpenAI 协议的 token 用量。
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatCompletionResponse 非流式 chat/completions 响应（只取需要的字段）。
type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

// chatStreamChunk 流式 SSE data 帧。
type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

// chatUpstream 一次发往 provider 的 chat/completions 请求。
type chatUpstream struct {
	Model           string            `json:"model"`
	Messages        []chatWireMessage `json:"messages"`
	Temperature     float64           `json:"temperature"`
	MaxTokens       int               `json:"max_tokens,omitempty"`
	Stream          bool              `json:"stream"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"` // 思考力度（empty = 不发送）
}

// chatModelInfo 模型列表中的单个模型（附视觉/推理能力粗判）。
type chatModelInfo struct {
	ID        string `json:"id"`
	Vision    bool   `json:"vision"`
	Reasoning bool   `json:"reasoning"`
}

// doProviderRequest 向 provider 的 /v1 端点发请求（path 如 "/models"、"/chat/completions"）。
func doProviderRequest(ctx context.Context, client *http.Client, p config.Provider, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, openaiV1URL(p.BaseURL)+path, rdr)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

// openaiV1URL 组装 OpenAI 兼容基址：去尾部斜杠与已有的 /v1（与 langchaingo 行为一致）。
func openaiV1URL(baseURL string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	b = strings.TrimSuffix(b, "/v1")
	return b + "/v1"
}

// writeSSE 输出一个命名事件（data 为 JSON 单行）。
func writeSSE(w io.Writer, event string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
}

// countChatImages 本次请求携带的图片总数（日志字段）。
func countChatImages(messages []chatMessage) int {
	n := 0
	for _, m := range messages {
		n += len(m.Images)
	}
	return n
}

// messagesText 拼接全部消息文本（估算 prompt token 用）。
func messagesText(messages []chatMessage) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(m.Content)
	}
	return b.String()
}

// estimatePromptTokens 端点未回传 usage 时的 prompt 估算：文本按字符数，
// 图片各端点计费口径不一，统一按固定额度计入（仅用于统计展示，不追求精确）。
func estimatePromptTokens(messages []chatMessage) int {
	return estimateTokens(messagesText(messages)) + countChatImages(messages)*imageTokenAllowance
}

// imageTokenAllowance 单张图片计入估算的 token 数。
const imageTokenAllowance = 512

// estimateTokens 字符数粗估 token：ASCII 约 4 字符/token，其余（如中文）约 1 字符/token。
func estimateTokens(s string) int {
	var ascii, other int
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return ascii/4 + other
}

// snippet 截取响应体片段用于错误提示（压掉换行与多余空白）。
func snippet(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len([]rune(s)) > 200 {
		s = string([]rune(s)[:200]) + "…"
	}
	if s == "" {
		s = "(空响应)"
	}
	return s
}

func errStringOr(err error, def string) string {
	if err == nil {
		return def
	}
	return err.Error()
}

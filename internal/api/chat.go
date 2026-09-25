// chat.go 对话子功能后端：直连 OpenAI 兼容端点的四个能力——
//  1. 模型列表：GET {base-url}/v1/models（供前端选择模型）；
//  2. 流式探测：发一条最小 chat/completions（stream=true）判断端点是否支持 SSE；
//  3. 对话转发：chat/completions 以 SSE 事件（meta/delta/done/error）透传给浏览器，
//     端点不支持流式或前端显式关闭时回退普通调用一次性下发；
//  4. 图片输入：messages[].images 以 base64 随请求上传，转成 OpenAI 多模态
//     content 片段（text + image_url data URL）后转发，仅对视觉模型开放。
//
// 统计落库与 io-log 通过 Factory 的导出方法埋点，与工作流路径保持一致。
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tmc/langchaingo/llms"

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
	Vision    bool   `json:"vision"`    // 图像输入（视觉）
	Reasoning bool   `json:"reasoning"` // 声明推理/思考能力
}

// visionNameKeywords 模型名中的视觉系关键词（端点元数据缺失时的启发式判定）。
var visionNameKeywords = []string{
	"-vl", "vl-", "vision", "visual", "multimodal",
	"llava", "internvl", "cogvlm", "pixtral", "qvq", "minicpm-v", "glm-4v", "gpt-4o", "kimi-v",
}

// reasoningNameKeywords 模型名中的推理系关键词。
var reasoningNameKeywords = []string{
	"thinking", "reasoner", "reasoning", "-r1", "r1-", "qwq", "strawberry",
}

// reasoningNameTokens 需整段命中的版本号 token（o1/o3/o4 系列），避免误伤普通名称。
var reasoningNameTokens = []string{"o1", "o3", "o4"}

// detectModelCaps 粗判模型是否有视觉/推理能力：
// 先查 /v1/models 元数据里各家端点的私有字段（supported_modalities、architecture、
// meta、capabilities、supported_parameters 等子树，序列化后按关键词匹配），
// 再回退模型名关键词启发。本地端点常常两类信息都有缺失，两者互补。
func detectModelCaps(id string, entry map[string]any) (vision, reasoning bool) {
	for _, key := range []string{"supported_modalities", "architecture", "meta", "capabilities", "supported_parameters", "details"} {
		sub, ok := entry[key]
		if !ok || sub == nil {
			continue
		}
		b, err := json.Marshal(sub)
		if err != nil {
			continue
		}
		s := strings.ToLower(string(b))
		if strings.Contains(s, "image") || strings.Contains(s, "vision") || strings.Contains(s, "multimodal") {
			vision = true
		}
		if strings.Contains(s, "reasoning") || strings.Contains(s, "thinking") {
			reasoning = true
		}
	}
	lower := strings.ToLower(id)
	for _, kw := range visionNameKeywords {
		if strings.Contains(lower, kw) {
			vision = true
			break
		}
	}
	for _, kw := range reasoningNameKeywords {
		if strings.Contains(lower, kw) {
			reasoning = true
			break
		}
	}
	if !reasoning {
		for _, tok := range strings.FieldsFunc(lower, func(r rune) bool {
			return r == '-' || r == '.' || r == '_' || r == '/' || r == ':' || r == ' '
		}) {
			if slices.Contains(reasoningNameTokens, tok) {
				reasoning = true
				break
			}
		}
	}
	return vision, reasoning
}

// handleChatModels godoc
// @Summary      拉取 provider 的模型列表
// @Description  代理 OpenAI 兼容端点 GET {base-url}/v1/models，附视觉/推理能力粗判，供对话子功能选择模型（query: provider）
// @Tags         chat
// @Produce      json
// @Param        provider  query  string  true  "provider 名（ai.yml 中声明）"
// @Success      200  {object}  map[string]any  "provider / base_url / models[]（chatModelInfo）"
// @Failure      400  {object}  apiError
// @Failure      502  {object}  apiError
// @Router       /api/providers/models [get]
func (s *Server) handleChatModels(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("provider")
	p, ok := s.factory.Provider(name)
	if !ok {
		fail(w, http.StatusBadRequest, "provider 未在 ai.yml 中声明: "+name)
		return
	}
	client := s.factory.HTTPClient(p)
	client.Timeout = 15 * time.Second // 列表接口不应等生成超时
	resp, err := doProviderRequest(r.Context(), client, p, http.MethodGet, "/models", nil)
	if err != nil {
		fail(w, http.StatusBadGateway, "请求模型列表失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		fail(w, http.StatusBadGateway, "读取模型列表失败: "+err.Error())
		return
	}
	if resp.StatusCode != http.StatusOK {
		fail(w, http.StatusBadGateway, fmt.Sprintf("模型列表 HTTP %d: %s", resp.StatusCode, snippet(body)))
		return
	}
	var doc struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		fail(w, http.StatusBadGateway, "解析模型列表失败: "+err.Error())
		return
	}
	models := make([]chatModelInfo, 0, len(doc.Data))
	for _, raw := range doc.Data {
		var entry map[string]any
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		id, _ := entry["id"].(string)
		if id == "" {
			continue
		}
		vision, reasoning := detectModelCaps(id, entry)
		models = append(models, chatModelInfo{ID: id, Vision: vision, Reasoning: reasoning})
	}
	slices.SortFunc(models, func(a, b chatModelInfo) int {
		return strings.Compare(strings.ToLower(a.ID), strings.ToLower(b.ID))
	})
	respond(w, http.StatusOK, map[string]any{
		"provider":   p.Name,
		"base_url":   p.BaseURL,
		"models":     models,
		"fetched_at": time.Now().Format(time.RFC3339),
	})
}

// handleChatStreamCheck godoc
// @Summary      探测 provider 是否支持流式返回
// @Description  向 chat/completions 发一条最小流式请求（max_tokens=1），按 SSE 首帧判断端点是否支持流式（query: provider、model）
// @Tags         chat
// @Produce      json
// @Param        provider  query  string  true   "provider 名"
// @Param        model     query  string  true   "模型名"
// @Success      200  {object}  map[string]any  "provider / model / stream_supported / detail / latency_ms"
// @Failure      400  {object}  apiError
// @Router       /api/providers/stream-check [get]
func (s *Server) handleChatStreamCheck(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("provider")
	model := r.URL.Query().Get("model")
	p, ok := s.factory.Provider(name)
	if !ok {
		fail(w, http.StatusBadRequest, "provider 未在 ai.yml 中声明: "+name)
		return
	}
	if model == "" {
		fail(w, http.StatusBadRequest, "缺少 model 参数")
		return
	}
	// 探测用短超时，避免界面长时间等待
	client := s.factory.HTTPClient(p)
	client.Timeout = 20 * time.Second

	start := time.Now()
	supported, detail := probeStream(r.Context(), client, p, model)
	respond(w, http.StatusOK, map[string]any{
		"provider":         p.Name,
		"model":            model,
		"stream_supported": supported,
		"detail":           detail,
		"latency_ms":       time.Since(start).Milliseconds(),
		"checked_at":       time.Now().Format(time.RFC3339),
	})
}

// probeStream 发一条 stream=true 的最小请求：2xx 且能读到 data: 帧 → 支持流式。
func probeStream(ctx context.Context, client *http.Client, p config.Provider, model string) (bool, string) {
	payload := chatUpstream{
		Model:    model,
		Messages: wireMessages([]chatMessage{{Role: "user", Content: "ping"}}),
		Stream:   true,
	}
	resp, err := doProviderRequest(ctx, client, p, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	// 读取首个 data: 帧即判定（限制总量，防止异常端点刷屏）
	ok, detail := scanSSEHead(bufio.NewReader(resp.Body), 8<<10)
	if ok {
		return true, "SSE 流式响应正常"
	}
	return false, detail
}

// scanSSEHead 扫描响应头部：读到第一个含 choices 的 data 帧即认定支持流式。
func scanSSEHead(br *bufio.Reader, limit int64) (bool, string) {
	var read int
	for int64(read) < limit {
		line, err := br.ReadString('\n')
		read += len(line)
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" || strings.Contains(payload, "choices") {
				return true, ""
			}
		}
		if err != nil {
			return false, "未读到 SSE data 帧（响应可能不是流式格式）: " + errStringOr(err, "EOF")
		}
	}
	return false, "SSE 帧头超出读取上限"
}

// 对话图片默认上限（子功能 chat.max-images / chat.max-image-mb 可覆盖）。
const (
	defaultChatMaxImages  = 4
	defaultChatMaxImageMB = 8
	// 请求体硬上限：图片以 base64 内联（膨胀约 1.34 倍），按默认 4 张 × 8MB 量级放宽。
	maxChatBodyBytes = 64 << 20
)

// allowedChatImageMimes 对话图片允许的媒体类型（OpenAI 兼容端点的普遍集合）。
var allowedChatImageMimes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/webp": true,
	"image/gif": true, "image/bmp": true,
}

// chatMaxImages / chatMaxImageMB 取子功能图片限制（未配置时回退默认值）。
func chatMaxImages(f config.Feature) int {
	if f.Chat.MaxImages > 0 {
		return f.Chat.MaxImages
	}
	return defaultChatMaxImages
}

func chatMaxImageMB(f config.Feature) int {
	if f.Chat.MaxImageMB > 0 {
		return f.Chat.MaxImageMB
	}
	return defaultChatMaxImageMB
}

// validateChatImages 校验待发送图片：仅 user 消息可附图，并限制单条数量、
// 单张体积与媒体类型；同时就地洗掉客户端误带的 data URL 前缀并剔除空数据项。
func validateChatImages(messages []chatMessage, maxImages, maxBytes int) error {
	for i := range messages {
		m := &messages[i]
		if len(m.Images) == 0 {
			continue
		}
		if m.Role != "user" {
			return fmt.Errorf("图片只能随 user 消息发送（第 %d 条为 %s）", i+1, m.Role)
		}
		if len(m.Images) > maxImages {
			return fmt.Errorf("单条消息最多 %d 张图片，收到 %d 张", maxImages, len(m.Images))
		}
		cleaned := make([]chatImage, 0, len(m.Images))
		for j, img := range m.Images {
			mime := strings.ToLower(strings.TrimSpace(img.Mime))
			if !allowedChatImageMimes[mime] {
				return fmt.Errorf("第 %d 条消息第 %d 张图片类型不支持：%q（支持 png/jpeg/webp/gif/bmp）", i+1, j+1, img.Mime)
			}
			data := stripDataURLPrefix(img.Data)
			if data == "" {
				continue
			}
			if len(data)*3/4 > maxBytes { // base64 长度换算解码后字节（保守估计）
				return fmt.Errorf("第 %d 条消息第 %d 张图片超过 %dMB 上限", i+1, j+1, maxBytes>>20)
			}
			if err := decodeBase64Len(data); err != nil {
				return fmt.Errorf("第 %d 条消息第 %d 张图片不是合法 base64: %w", i+1, j+1, err)
			}
			img.Data = data
			img.Mime = mime
			if img.Name == "" {
				img.Name = "image-" + strconv.Itoa(j+1)
			}
			cleaned = append(cleaned, img)
		}
		if len(cleaned) == 0 {
			return fmt.Errorf("第 %d 条消息的图片内容为空", i+1)
		}
		m.Images = cleaned
	}
	return nil
}

// stripDataURLPrefix 去掉误传的 "data:image/png;base64," 前缀（图片已统一走 mime 字段）。
func stripDataURLPrefix(data string) string {
	if !strings.HasPrefix(data, "data:") {
		return data
	}
	if idx := strings.Index(data, ","); idx >= 0 {
		return data[idx+1:]
	}
	return data
}

// decodeBase64Len 流式解码以校验 base64 合法性（不保留解码结果，避免多占用一份内存）。
func decodeBase64Len(data string) error {
	r := base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))
	_, err := io.Copy(io.Discard, r)
	return err
}

// wireMessages 将内部消息转为 OpenAI 协议结构（带图的消息展开为 content 数组）。
func wireMessages(messages []chatMessage) []chatWireMessage {
	out := make([]chatWireMessage, 0, len(messages))
	for _, m := range messages {
		if len(m.Images) == 0 {
			out = append(out, chatWireMessage{Role: m.Role, Content: m.Content})
			continue
		}
		parts := make([]chatPart, 0, len(m.Images)+1)
		if strings.TrimSpace(m.Content) != "" {
			parts = append(parts, chatPart{Type: "text", Text: m.Content})
		}
		for _, img := range m.Images {
			parts = append(parts, chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: imageDataURL(img)}})
		}
		out = append(out, chatWireMessage{Role: m.Role, Content: parts})
	}
	return out
}

// imageDataURL 拼装 data URL（端点接受的 image_url 形式，避开服务端落盘/外链）。
func imageDataURL(img chatImage) string {
	return "data:" + img.Mime + ";base64," + img.Data
}

// handleChatSend godoc
// @Summary      发送对话消息（SSE 流式）
// @Description  将消息列表转发到所选 provider 的 chat/completions；默认以 SSE 逐块下发（事件 meta/delta/done/error），端点不支持流式时自动降级为一次性 JSON（附 stream_fallback/note），stream=false 时直接一次性 JSON 返回。messages[].images 可附带图片（base64，仅视觉模型与 user 消息）
// @Tags         chat
// @Accept       json
// @Produce      application/json,text/event-stream
// @Param        body  body  chatSendRequest  true  "对话请求"
// @Success      200  {string}  string  "SSE 事件流或完整响应"
// @Failure      400  {object}  apiError
// @Failure      404  {object}  apiError
// @Router       /api/chat/send [post]
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	var req chatSendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxChatBodyBytes)).Decode(&req); err != nil {
		// 图片以 base64 内联，超限时只会读到截断的 JSON，这里给出可行动的提示
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			fail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("请求体超过 %dMB 上限，请减少图片数量或改用体积更小的图片", maxChatBodyBytes>>20))
			return
		}
		fail(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	feature, ok := s.cfg.Feature(req.FeatureID)
	if !ok || !feature.IsChat() {
		fail(w, http.StatusNotFound, "对话子功能未启用或不存在: "+req.FeatureID)
		return
	}
	if len(req.Messages) == 0 {
		fail(w, http.StatusBadRequest, "messages 为空")
		return
	}

	// provider：请求指定 > 子功能默认 > 全局默认
	providerName := req.Provider
	if providerName == "" {
		providerName = feature.Chat.DefaultProvider
	}
	if providerName == "" {
		providerName = s.cfg.LLM.DefaultProvider
	}
	p, ok := s.factory.Provider(providerName)
	if !ok {
		fail(w, http.StatusBadRequest, "provider 未在 ai.yml 中声明: "+providerName)
		return
	}
	st := feature.Steps["chat"].WithDefaults(s.chatDefault(feature))
	model := req.Model
	if model == "" {
		model = st.Model
	}

	// 历史裁剪：只保留最近 max-history 条（system 由服务端统一注入）
	messages := trimChatHistory(req.Messages, feature.Chat.MaxHistory)
	if err := validateChatImages(messages, chatMaxImages(feature), chatMaxImageMB(feature)<<20); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if feature.Chat.SystemPrompt != "" {
		messages = append([]chatMessage{{Role: "system", Content: feature.Chat.SystemPrompt}}, messages...)
	}

	temperature := st.Temp()
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	maxTokens := st.MaxTokens
	if req.MaxTokens > 0 {
		maxTokens = req.MaxTokens
	}
	stream := s.chatStreamDefault(feature)
	if req.Stream != nil {
		stream = *req.Stream
	}

	slog.Debug("chat send",
		slog.String("feature", feature.ID),
		slog.String("provider", providerName),
		slog.String("model", model),
		slog.Int("images", countChatImages(messages)),
		slog.Bool("stream", stream))

	if stream {
		s.streamChat(w, r, p, model, messages, temperature, maxTokens, feature.Chat.ReasoningEffort)
		return
	}
	s.collectChat(w, r, p, model, messages, temperature, maxTokens, feature.Chat.ReasoningEffort)
}

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

// chatDefault 对话子功能的步骤回退值（provider/模型/参数取全局配置）。
func (s *Server) chatDefault(f config.Feature) config.Step {
	provider := f.Chat.DefaultProvider
	if provider == "" {
		provider = s.cfg.LLM.DefaultProvider
	}
	temp := s.cfg.LLM.Temperature
	return config.Step{
		Provider:    provider,
		Model:       envOr("HARNESS_DEFAULT_MODEL", "qwen3-vl-8b"),
		Temperature: &temp,
		MaxTokens:   s.cfg.LLM.MaxTokens,
	}
}

// chatStreamDefault 流式开关：子功能 stream-default 优先，未配置跟随全局 stream-output。
func (s *Server) chatStreamDefault(f config.Feature) bool {
	if f.Chat.StreamDefault != nil {
		return *f.Chat.StreamDefault
	}
	return s.cfg.LLM.StreamOutput
}

// trimChatHistory 保留最近 max 条消息（去掉文本与图片均为空的），max<=0 时不限制数量。
// 只图片无文本的 user 消息是合法的多模态输入，不能当空消息丢弃。
func trimChatHistory(in []chatMessage, max int) []chatMessage {
	cleaned := make([]chatMessage, 0, len(in))
	for _, m := range in {
		if strings.TrimSpace(m.Content) == "" && len(m.Images) == 0 {
			continue
		}
		if m.Role != "user" && m.Role != "assistant" {
			continue // system 由服务端注入，客户端传入一律忽略
		}
		cleaned = append(cleaned, m)
	}
	if max > 0 && len(cleaned) > max {
		cleaned = cleaned[len(cleaned)-max:]
	}
	return cleaned
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

// toMessageContents 将对话消息转为 langchaingo 结构（仅供 io-log 记录）。
// 图片只记录类型与体积的占位文本，不把 base64 写入日志。
func toMessageContents(messages []chatMessage) []llms.MessageContent {
	out := make([]llms.MessageContent, 0, len(messages))
	for _, m := range messages {
		role := llms.ChatMessageTypeHuman
		switch m.Role {
		case "system":
			role = llms.ChatMessageTypeSystem
		case "assistant":
			role = llms.ChatMessageTypeAI
		}
		parts := []llms.ContentPart{llms.TextContent{Text: m.Content}}
		for _, img := range m.Images {
			parts = append(parts, llms.TextContent{Text: imageLogMarker(img)})
		}
		out = append(out, llms.MessageContent{Role: role, Parts: parts})
	}
	return out
}

// imageLogMarker io-log 中的图片占位描述。
func imageLogMarker(img chatImage) string {
	return fmt.Sprintf("<image name=%s mime=%s base64_bytes=%d>", img.Name, img.Mime, len(img.Data))
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

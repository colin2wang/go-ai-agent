package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/tmc/langchaingo/llms"

	"go-ai-agent/internal/config"
)

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

// chatDefault 对话子功能的步骤回退值（provider/模型/参数取全局配置）。
func (s *Server) chatDefault(f config.Feature) config.Step {
	provider := f.Chat.DefaultProvider
	if provider == "" {
		provider = s.cfg.LLM.DefaultProvider
	}
	temp := s.cfg.LLM.Temperature
	return config.Step{
		Provider:    provider,
		Model:       envOr("HARNESS_DEFAULT_MODEL", "qwen/qwen3-vl-8b"),
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

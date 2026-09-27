package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"go-ai-agent/internal/config"
	"go-ai-agent/internal/stats"
)

// modelCaps 一次真实接口探测得到的能力结果（缓存值）。
// 流式为端点级结论；视觉/推理优先真实调用，探测失败时回退元数据/名称启发。
type modelCaps struct {
	Vision      bool   `json:"vision"`
	Reasoning   bool   `json:"reasoning"`
	Stream      bool   `json:"stream"`
	StreamNote  string `json:"stream_note,omitempty"` // 流式探测说明（失败原因等）
	ProbedAt    string `json:"probed_at"`             // RFC3339，探测时间
	ProbedVLM   bool   `json:"probed_vlm"`            // 视觉是否经过真实图片调用验证
	ProbedJSON  bool   `json:"probed_json"`           // 推理是否经过真实 JSON 调用验证
	HeuristicOK bool   `json:"heuristic_ok"`          // 启发式兜底结论（探测失败时使用）
}

// modelCapsCache 模型能力探测缓存：provider|model → 结果。
// 只要模型不变就复用，避免每次切换模型都重复探测（进程内缓存，
// 启动时从 SQLite model_caps 预热，探测后同步落库）。
type modelCapsCache struct {
	mu    sync.RWMutex
	items map[string]modelCaps
}

func newModelCapsCache() *modelCapsCache {
	return &modelCapsCache{items: map[string]modelCaps{}}
}

func capsKey(provider, model string) string { return provider + "|" + model }

// get 返回缓存的能力结果与是否存在。
func (c *modelCapsCache) get(provider, model string) (modelCaps, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.items[capsKey(provider, model)]
	return v, ok
}

// put 写入/覆盖某个模型的能力结果。
func (c *modelCapsCache) put(provider, model string, caps modelCaps) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[capsKey(provider, model)] = caps
}

// warm 启动预热：仅在内存无该键时写入（从 SQLite model_caps 行加载）。
func (c *modelCapsCache) warm(provider, model string, row stats.CapsRow) {
	caps := modelCaps{
		Vision: row.Vision, Reasoning: row.Reasoning, Stream: row.Stream, StreamNote: row.StreamNote,
		ProbedAt:    time.UnixMilli(row.ProbedAtMS).Format(time.RFC3339),
		ProbedVLM:   row.ProbedVLM,
		ProbedJSON:  row.ProbedJSON,
		HeuristicOK: row.HeuristicOK,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[capsKey(provider, model)]; !ok {
		c.items[capsKey(provider, model)] = caps
	}
}

// persistCaps 把探测结果同步写入 SQLite（未启用统计库时 no-op；失败仅记日志，
// 不影响本次请求响应）。
func (s *Server) persistCaps(provider, model string, caps modelCaps) {
	if s.recorder == nil {
		return
	}
	probedAtMS := time.Now().UnixMilli()
	if t, err := time.Parse(time.RFC3339, caps.ProbedAt); err == nil {
		probedAtMS = t.UnixMilli()
	}
	if err := s.recorder.SaveCaps(stats.CapsRow{
		Provider: provider, Model: model,
		Vision: caps.Vision, Reasoning: caps.Reasoning, Stream: caps.Stream, StreamNote: caps.StreamNote,
		ProbedVLM: caps.ProbedVLM, ProbedJSON: caps.ProbedJSON, HeuristicOK: caps.HeuristicOK,
		ProbedAtMS: probedAtMS,
	}); err != nil {
		slog.Warn("能力探测结果持久化失败", slog.String("provider", provider),
			slog.String("model", model), slog.String("error", err.Error()))
	}
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
		// 已有真实探测缓存时以缓存为准，否则退回元数据 + 名称启发
		vision, reasoning := detectModelCaps(id, entry)
		if caps, ok := s.capsCache.get(name, id); ok {
			vision, reasoning = caps.Vision, caps.Reasoning
		}
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
	// 顺手写入能力缓存：流式结论端点级通用，视觉/推理保持缓存原值（未探测则启发式兜底）
	if caps, ok := s.capsCache.get(name, model); ok {
		caps.Stream, caps.StreamNote = supported, detail
		s.capsCache.put(name, model, caps)
		s.persistCaps(name, model, caps)
	} else {
		hv, hr := detectModelCaps(model, nil)
		caps := modelCaps{
			Vision: hv, Reasoning: hr, Stream: supported, StreamNote: detail,
			ProbedAt: time.Now().Format(time.RFC3339), HeuristicOK: true,
		}
		s.capsCache.put(name, model, caps)
		s.persistCaps(name, model, caps)
	}
	respond(w, http.StatusOK, map[string]any{
		"provider":         p.Name,
		"model":            model,
		"stream_supported": supported,
		"detail":           detail,
		"latency_ms":       time.Since(start).Milliseconds(),
		"checked_at":       time.Now().Format(time.RFC3339),
	})
}

// probeVisionColorWords 探测用纯色图（品红/magenta 系）的可接受答案词（小写匹配）。
// 刻意排除「红/蓝/绿/黑/白/黄」等盲猜高频词：只有真正「看懂」图片的模型才可能
// 答出品红/紫/粉这一色系，纯文本模型忽略图片后无从得知，借此消除「端点收下
// image 片段却并不具备视觉能力」的假阳性。
var probeVisionColorWords = []string{
	"品红", "洋红", "玫红", "紫", "粉", "magenta", "fuchsia", "purple", "pink",
}

// solidColorPNGBase64 生成一张 256×256 纯色 PNG 并返回纯 base64（不含 data: 前缀）。
// 纯色图经 PNG 无损压缩后体积极小，适合内联进探测请求。
func solidColorPNGBase64(r, g, b uint8) string {
	const sz = 256
	img := image.NewRGBA(image.Rect(0, 0, sz, sz))
	c := color.RGBA{R: r, G: g, B: b, A: 255}
	for y := 0; y < sz; y++ {
		for x := 0; x < sz; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img) // 内存编码不会失败
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// probeVision 真实调用一次多模态请求判定视觉能力：发送一张品红纯色图，
// 仅当模型回答命中该色系时才认定具备视觉（可判别，消除假阳性）。
// 返回 (视觉, 探测错误)：探测错误非空表示端点未正常应答（拒绝图片/出错/响应异常），
// 调用方应回退名字/元数据启发式；错误为空表示探测成功，视觉结论以回答内容为准。
func probeVision(ctx context.Context, client *http.Client, p config.Provider, model string) (bool, string) {
	payload := chatUpstream{
		Model: model,
		Messages: []chatWireMessage{{
			Role: "user",
			Content: []chatPart{
				{Type: "image_url", ImageURL: &chatImageURL{URL: "data:image/png;base64," + solidColorPNGBase64(255, 0, 255)}},
				{Type: "text", Text: "这是一张纯色图片。请只回答它的颜色名称（一个词），不要解释或添加其他文字。"},
			},
		}},
		MaxTokens: 32,
	}
	resp, err := doProviderRequest(ctx, client, p, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var out chatCompletionResponse
	if json.Unmarshal(body, &out) != nil || len(out.Choices) == 0 {
		return false, "响应缺少 choices"
	}
	answer := strings.ToLower(out.Choices[0].Message.Content)
	for _, w := range probeVisionColorWords {
		if strings.Contains(answer, w) {
			return true, ""
		}
	}
	return false, ""
}

// probeReasoning 非流式调用一次，检查 reasoning_content / reasoning_tokens
// 是否出现，判定模型是否输出思考链。
func probeReasoning(ctx context.Context, client *http.Client, p config.Provider, model string) (bool, string) {
	payload := chatUpstream{
		Model:     model,
		Messages:  wireMessages([]chatMessage{{Role: "user", Content: "你好"}}),
		MaxTokens: 256, // 思考型模型可能先消耗推理 token，给足预算
	}
	resp, err := doProviderRequest(ctx, client, p, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	// usage 结构因端点而异（reasoning_tokens 可能是顶层字段），宽松解析
	var raw struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(body, &raw) != nil || len(raw.Choices) == 0 {
		return false, "响应缺少 choices"
	}
	if raw.Choices[0].Message.ReasoningContent != "" {
		return true, ""
	}
	if n, ok := raw.Usage["reasoning_tokens"].(float64); ok && n > 0 {
		return true, ""
	}
	if cd, ok := raw.Usage["completion_tokens_details"].(map[string]any); ok {
		if n, ok := cd["reasoning_tokens"].(float64); ok && n > 0 {
			return true, ""
		}
	}
	return false, ""
}

// probeModelCaps 综合探测一个模型的能力：视觉/推理各发一次真实请求，
// 流式发一次最小流式请求；全部走通才写缓存（网络失败时回退启发式并标注）。
func (s *Server) probeModelCaps(ctx context.Context, p config.Provider, model string) modelCaps {
	client := s.factory.HTTPClient(p)
	client.Timeout = 30 * time.Second

	// 启发式（名字/元数据）先算好：视觉真实探测与已知 VL 名取并集，避免把
	// 真视觉模型因“回答未命中颜色词”误判为非视觉；推理回退时也用得到。
	hv, hr := detectModelCaps(model, nil)

	caps := modelCaps{ProbedAt: time.Now().Format(time.RFC3339)}
	visionOK, visionErr := probeVision(ctx, client, p, model)
	if visionErr == "" {
		// 探测成功：以“是否真看懂图片”为准，已知 VL 名保底为视觉。
		caps.Vision, caps.ProbedVLM = visionOK || hv, true
	} else {
		caps.Vision = hv // 端点拒绝图片/出错，回退启发式
	}
	reasonOK, reasonErr := probeReasoning(ctx, client, p, model)
	if reasonErr == "" {
		caps.Reasoning, caps.ProbedJSON = reasonOK, true
	} else {
		caps.Reasoning = hr
	}
	caps.Stream, caps.StreamNote = probeStream(ctx, client, p, model)

	// 两项真实探测均成功→ HeuristicOK=true；任一探测未能完成→ false（结论含启发成分）。
	caps.HeuristicOK = visionErr == "" && reasonErr == ""
	return caps
}

// probeMu 串行化探测：避免并发请求同一未缓存模型时重复打端点。
// handleModelCaps godoc
// @Summary      探测并缓存模型能力
// @Description  对指定 provider+model 真实调用接口检测视觉/推理/流式能力并缓存；模型不变时直接返回缓存结果（query: provider、model、force=1 跳过缓存强制重测）
// @Tags         chat
// @Produce      json
// @Param        provider  query  string  true   "provider 名"
// @Param        model     query  string  true   "模型名"
// @Param        force     query  string  false  "force=1 强制重新探测"
// @Success      200  {object}  map[string]any  "provider / model / vision / reasoning / stream / cached / probed_at"
// @Failure      400  {object}  apiError
// @Router       /api/providers/caps [get]
func (s *Server) handleModelCaps(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("provider")
	model := r.URL.Query().Get("model")
	force := r.URL.Query().Get("force") == "1"
	p, ok := s.factory.Provider(name)
	if !ok {
		fail(w, http.StatusBadRequest, "provider 未在 ai.yml 中声明: "+name)
		return
	}
	if model == "" {
		fail(w, http.StatusBadRequest, "缺少 model 参数")
		return
	}
	if !force {
		if caps, ok := s.capsCache.get(name, model); ok {
			respond(w, http.StatusOK, capsResponse(p, model, caps, true))
			return
		}
	}
	// 单飞：同 key 并发请求只探测一次（先到者写缓存，其余读缓存）
	s.probeLock.Lock()
	if caps, ok := s.capsCache.get(name, model); ok && !force {
		s.probeLock.Unlock()
		respond(w, http.StatusOK, capsResponse(p, model, caps, true))
		return
	}
	caps := s.probeModelCaps(r.Context(), p, model)
	s.capsCache.put(name, model, caps)
	s.probeLock.Unlock()
	s.persistCaps(name, model, caps) // 真实探测结果落库，重启免重测
	respond(w, http.StatusOK, capsResponse(p, model, caps, false))
}

// capsResponse 组装能力探测响应。
func capsResponse(p config.Provider, model string, caps modelCaps, cached bool) map[string]any {
	return map[string]any{
		"provider":     p.Name,
		"model":        model,
		"vision":       caps.Vision,
		"reasoning":    caps.Reasoning,
		"stream":       caps.Stream,
		"stream_note":  caps.StreamNote,
		"probed_vlm":   caps.ProbedVLM,
		"probed_json":  caps.ProbedJSON,
		"heuristic_ok": caps.HeuristicOK,
		"probed_at":    caps.ProbedAt,
		"cached":       cached,
	}
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

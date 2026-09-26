// Package nodes 实现各业务节点：每个节点独立声明 provider+model，互不影响。
package nodes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/tmc/langchaingo/llms"

	"go-ai-agent/internal/config"
	"go-ai-agent/internal/llm"
	"go-ai-agent/internal/prompt"
	"go-ai-agent/internal/tmpl"
)

// State 全流程共享状态（泛型 StateGraph[State] 的载体）。
// 定义在 nodes 包以避免 graph ↔ nodes 循环引用；graph 包用类型别名引用。
type State struct {
	ImagePath    string         // 单页输入（工资单截图路径）
	Pages        []string       // 多页输入（扫描版图书目录页，按选择顺序）
	Title        string         // 识别出的书名（图书目录场景）
	OCRText      string         // OCR 节点输出的结构化 JSON（多页时为各页拼接）
	PageOCR      []string       // 每一页的原始 OCR 输出
	Entries      []TOCEntry     // 合并后的目录条目（图书目录场景）
	Structured   map[string]any // 结构化字段（工资单字段 / 目录条目汇总）
	Translation  string         // 翻译结果
	ReviewNotes  []string       // 审读发现的问题 / 降级提示
	ExpertReport string         // 专家分析
	FinalReport  string         // 最终输出（汇总报告 / 模板渲染后的目录）
	Err          error          // 最近一次节点错误（供条件边重试判断）
	RetryCount   map[string]int // 各节点已重试次数
}

// Deps 节点共享依赖（由某条子功能的工作流配置构造）。
type Deps struct {
	Factory *llm.Factory
	// Steps 按节点名提供 provider+model+覆盖项；未声明的节点回退 Default。
	Steps   map[string]config.Step
	Default config.Step // provider 已回退展开
	Stream  bool
	// MaxRetries 条件边回退重试上限（来自子功能 workflow.max-retries）。
	MaxRetries int
	// Workflow 工作流类型（来自子功能 workflow.type），供编排层选择图。
	Workflow string
	// Template 模板引擎（本地渲染步骤使用，可为 nil）。
	Template *tmpl.Engine
	// Output 输出声明（模板名 / 外部模板目录 / 扩展名）。
	Output config.Output
}

// stepCfg 返回节点对应的步骤配置（未单独声明则回退 Default）。
func (d Deps) stepCfg(name string) config.Step {
	return d.Steps[name].WithDefaults(d.Default)
}

// call 封装一次 LLM 调用：取客户端 → 组装选项 → GenerateContent，返回回答文本。
func (d Deps) call(ctx context.Context, stepName, provider, model string,
	messages []llms.MessageContent, temperature float64, maxTokens int) (string, error) {

	chat, err := d.Factory.Get(provider, model)
	if err != nil {
		return "", err
	}
	acc := &llm.StreamAccumulator{}
	opts := llm.StreamOptions(d.Stream, acc, temperature, maxTokens)
	resp, err := chat.GenerateContent(ctx, messages, opts...)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("step %s: 空响应", stepName)
	}
	content := resp.Choices[0].Content
	// 流式累积器优先（部分后端流式下 Choices 内容可能为空）
	if d.Stream && content == "" {
		content = acc.AnswerText()
	}
	return content, nil
}

// OCR 视觉 OCR 节点：工资单图片（一张或多张）→ 结构化 JSON。
func OCR(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		st := d.stepCfg("ocr")
		// 工资单支持多张图片：优先用 Pages（多图上传），回退单张 ImagePath（CLI / 单图路径）。
		images := s.Pages
		if len(images) == 0 && s.ImagePath != "" {
			images = []string{s.ImagePath}
		}
		if len(images) == 0 {
			s.Err = fmt.Errorf("未收到任何工资单图片")
			return s, s.Err
		}
		// 全部图片放入同一条多模态消息，模型输出合并后的单份 JSON，
		// 下游 translate/proofread/expert 无需感知图片数量。
		// 注意：不能用 BinaryPart——langchaingo v0.1.14 会把它序列化为私有
		// {"type":"binary"} 格式，LM Studio 不识别导致图片被静默忽略、空回答。
		// 统一构造标准 OpenAI image_url（data URL），与 chat 路径 wire 格式一致。
		parts := make([]llms.ContentPart, 0, len(images)+1)
		for i, p := range images {
			img, err := os.ReadFile(p)
			if err != nil {
				s.Err = fmt.Errorf("读取第 %d 张图片失败: %w", i+1, err)
				return s, s.Err
			}
			parts = append(parts, llms.ImageURLContent{URL: imageDataURL(mimeOf(p), img)})
		}
		sys, usr := d.stepPrompt("ocr")
		parts = append(parts, llms.TextPart(usr))
		messages := []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, sys),
			{
				// 多模态消息必须显式声明 role，否则 langchaingo 报 "role not supported"
				Role:  llms.ChatMessageTypeHuman,
				Parts: parts,
			},
		}
		text, err := d.call(ctx, "ocr", st.Provider, st.Model, messages, st.Temp(), st.MaxTokens)
		if err != nil {
			s.Err = err
			return s, err
		}
		s.OCRText = text
		s.FinalReport = text // 提取+合并后的结果即最终输出（无后续分析/报告节点）
		// 尽力把 OCR JSON 解析到 Structured，供前端结构化展示（失败不影响主流程）。
		var fields map[string]any
		if err := json.Unmarshal([]byte(stripJSONFence(text)), &fields); err == nil {
			s.Structured = fields
		}
		s.Err = nil
		return s, nil
	}
}

// stepPrompt 返回步骤的系统/用户提示词：优先用 yml 的 system-prompt/user-prompt，
// 缺失时回退 internal/prompt 的内置默认（prompt.Defaults）。
func (d Deps) stepPrompt(name string) (system, user string) {
	st := d.stepCfg(name)
	system, user = st.SystemPrompt, st.UserPrompt
	if system == "" || user == "" {
		defSys, defUsr := prompt.Defaults(name)
		if system == "" {
			system = defSys
		}
		if user == "" {
			user = defUsr
		}
	}
	return
}

// Translate 翻译节点：复用同一工厂，仅切换 provider+model+prompt。
func Translate(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		st := d.stepCfg("translate")
		messages := []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, prompt.TranslateSystem),
			llms.TextParts(llms.ChatMessageTypeHuman, s.OCRText),
		}
		text, err := d.call(ctx, "translate", st.Provider, st.Model, messages, st.Temp(), st.MaxTokens)
		if err != nil {
			s.Err = err
			return s, err
		}
		s.Translation = text
		s.Err = nil
		return s, nil
	}
}

// Proofread 审读节点：校验字段，结果写入 ReviewNotes，并尽力解析 OCR JSON 到 Structured。
func Proofread(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		st := d.stepCfg("proofread")
		messages := []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, prompt.ProofreadSystem),
			llms.TextParts(llms.ChatMessageTypeHuman, s.OCRText),
		}
		text, err := d.call(ctx, "proofread", st.Provider, st.Model, messages, st.Temp(), st.MaxTokens)
		if err != nil {
			s.Err = err
			return s, err
		}
		var parsed struct {
			Valid  bool            `json:"valid"`
			Issues json.RawMessage `json:"issues"`
		}
		if err := json.Unmarshal([]byte(stripJSONFence(text)), &parsed); err != nil {
			s.Err = fmt.Errorf("proofread 输出非法 JSON: %w", err)
			return s, s.Err
		}
		notes, err := parseProofIssues(parsed.Issues)
		if err != nil {
			s.Err = fmt.Errorf("proofread issues 解析失败: %w", err)
			return s, s.Err
		}
		s.ReviewNotes = notes
		// 同步填充 Structured（验收关键字段用）
		var fields map[string]any
		if err := json.Unmarshal([]byte(stripJSONFence(s.OCRText)), &fields); err == nil {
			s.Structured = fields
		}
		s.Err = nil
		return s, nil
	}
}

// proofIssue issues 对象的单条形态（field + description）。
type proofIssue struct {
	Field       string `json:"field"`
	Description string `json:"description"`
}

// parseProofIssues 将 proofread 返回的 issues 解析为 []string（供下游渲染）。
// 兼容模型可能返回的多种形态，避免单形态强约束导致「非法 JSON」类报错：
//  1. 对象数组 [{field, description}]（提示词要求的标准形态）
//  2. 字符串数组 ["..."]
//  3. 单个字符串 "..."（或数字等标量）
func parseProofIssues(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	// 1) 对象数组（标准形态）
	var objs []proofIssue
	if json.Unmarshal(raw, &objs) == nil {
		out := make([]string, 0, len(objs))
		for _, it := range objs {
			if it.Field != "" {
				out = append(out, it.Field+": "+it.Description)
			} else {
				out = append(out, it.Description)
			}
		}
		return out, nil
	}
	// 2) 字符串数组
	var strs []string
	if json.Unmarshal(raw, &strs) == nil {
		return strs, nil
	}
	// 3) 单个标量
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return []string{single}, nil
	}
	return nil, fmt.Errorf("issues 形态无法识别: %s", string(bytes.TrimSpace(raw)))
}

// Expert 专家分析节点。
func Expert(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		st := d.stepCfg("expert")
		messages := []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, prompt.ExpertSystem),
			llms.TextParts(llms.ChatMessageTypeHuman, s.OCRText),
		}
		text, err := d.call(ctx, "expert", st.Provider, st.Model, messages, st.Temp(), st.MaxTokens)
		if err != nil {
			s.Err = err
			return s, err
		}
		s.ExpertReport = text
		s.Err = nil
		return s, nil
	}
}

// Analyze 汇总最终报告节点。
func Analyze(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		st := d.stepCfg("analyze")
		input := fmt.Sprintf("## OCR 结果\n%s\n\n## 翻译\n%s\n\n## 审读结论\n%v\n\n## 专家分析\n%s",
			s.OCRText, s.Translation, s.ReviewNotes, s.ExpertReport)
		messages := []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, prompt.AnalyzeSystem),
			llms.TextParts(llms.ChatMessageTypeHuman, input),
		}
		text, err := d.call(ctx, "analyze", st.Provider, st.Model, messages, st.Temp(), st.MaxTokens)
		if err != nil {
			s.Err = err
			return s, err
		}
		s.FinalReport = text
		s.Err = nil
		return s, nil
	}
}

// maxImageLongEdge 发送前将超长边图片等比缩放到此像素内。
// 扫描版图书目录/工资单常为高分辨率大图，本地 VLM（如 LM Studio 上的
// Qwen3-VL）对超大图片会静默丢弃、直接返回空回答（表现为 choices=1 但
// content 为空、耗时极短）。先验缩放到合理分辨率可显著降低视觉 token 量、
// 规避该问题，同时保持文本清晰可读。
const maxImageLongEdge = 1600

// imageDataURL 将图片字节编码为标准 OpenAI 多模态 data URL（image_url.url）。
// 发送前会：1) 按实际内容（magic bytes）推断 MIME，避免扩展名与内容不符导致
// 后端无法解码而静默返回空；2) 对超长边图片做等比缩小，降低视觉 token 量。
// 解码/缩放失败时回退为原图直接编码，交给后端决定。
func imageDataURL(declaredMime string, data []byte) string {
	mime := detectMime(data)
	if mime == "" {
		mime = declaredMime
	}

	// 仅对 stdlib 可解码的格式做缩小（png/jpeg/gif 已注册）。
	// webp/bmp 等无法 stdlib 解码时直接原样发送。
	out, outMime := data, mime
	switch mime {
	case "image/png", "image/jpeg", "image/gif":
		if src, _, err := image.Decode(bytes.NewReader(data)); err == nil {
			resized := downscale(src, maxImageLongEdge)
			var buf bytes.Buffer
			var encErr error
			switch mime {
			case "image/png":
				encErr = png.Encode(&buf, resized)
			case "image/jpeg":
				encErr = jpeg.Encode(&buf, resized, &jpeg.Options{Quality: 90})
			default: // gif：缩小后按 png 重编码（保留无损且体积更小）
				encErr = png.Encode(&buf, resized)
				outMime = "image/png"
			}
			if encErr == nil && buf.Len() > 0 {
				out = buf.Bytes()
			}
		}
	}
	return "data:" + outMime + ";base64," + base64.StdEncoding.EncodeToString(out)
}

// detectMime 按文件头 magic bytes 推断真实 MIME（比扩展名可靠）。
func detectMime(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 2 && data[0] == 'B' && data[1] == 'M':
		return "image/bmp"
	default:
		return ""
	}
}

// downscale 将图片等比缩放到 longEdge 像素内（仅在超出时缩放）。
// 采用盒式(box)采样平均，避免直接最近邻带来的锯齿，对 OCR 文本足够清晰。
func downscale(src image.Image, longEdge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	long := w
	if h > long {
		long = h
	}
	if long <= longEdge {
		return src
	}
	scale := float64(longEdge) / float64(long)
	dw := int(float64(w)*scale + 0.5)
	dh := int(float64(h)*scale + 0.5)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy0 := int(float64(y) / scale)
		sy1 := int(float64(y+1)/scale + 0.5)
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		if sy1 > h {
			sy1 = h
		}
		for x := 0; x < dw; x++ {
			sx0 := int(float64(x) / scale)
			sx1 := int(float64(x+1)/scale + 0.5)
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			if sx1 > w {
				sx1 = w
			}
			var r, g, bl, a uint64
			var n int
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					pr, pg, pb, pa := src.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					r += uint64(pr)
					g += uint64(pg)
					bl += uint64(pb)
					a += uint64(pa)
					n++
				}
			}
			if n > 0 {
				dst.SetRGBA(x, y, color.RGBA{
					R: uint8(r / uint64(n) >> 8),
					G: uint8(g / uint64(n) >> 8),
					B: uint8(bl / uint64(n) >> 8),
					A: uint8(a / uint64(n) >> 8),
				})
			}
		}
	}
	return dst
}

// mimeOf 按扩展名推断图片 MIME（默认 image/png），仅作回退用途。
func mimeOf(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".gif":
		return "image/gif"
	default:
		return "image/png"
	}
}

// stripJSONFence 去掉 LLM 输出可能携带的 ```json 围栏。
func stripJSONFence(s string) string {
	for _, fence := range []string{"```json", "```JSON", "```"} {
		if len(s) >= len(fence) && s[:len(fence)] == fence {
			s = s[len(fence):]
			break
		}
	}
	if len(s) > 0 && s[len(s)-3:] == "```" {
		s = s[:len(s)-3]
	}
	return s
}

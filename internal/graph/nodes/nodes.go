// Package nodes 实现各业务节点：每个节点独立声明 provider+model，互不影响。
package nodes

import (
	"context"
	"encoding/json"
	"fmt"
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

// OCR 视觉 OCR 节点：工资单截图 → 结构化 JSON。
func OCR(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		st := d.stepCfg("ocr")
		img, err := os.ReadFile(s.ImagePath)
		if err != nil {
			s.Err = err
			return s, err
		}
		messages := []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, prompt.OCRSystem),
			{
				// 多模态消息必须显式声明 role，否则 langchaingo 报 "role not supported"
				Role: llms.ChatMessageTypeHuman,
				Parts: []llms.ContentPart{
					llms.BinaryPart(mimeOf(s.ImagePath), img),
					llms.TextPart(prompt.OCRUser),
				},
			},
		}
		text, err := d.call(ctx, "ocr", st.Provider, st.Model, messages, st.Temp(), st.MaxTokens)
		if err != nil {
			s.Err = err
			return s, err
		}
		s.OCRText = text
		s.Err = nil
		return s, nil
	}
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
		var result struct {
			Valid  bool     `json:"valid"`
			Issues []string `json:"issues"`
		}
		if err := json.Unmarshal([]byte(stripJSONFence(text)), &result); err != nil {
			s.Err = fmt.Errorf("proofread 输出非法 JSON: %w", err)
			return s, s.Err
		}
		s.ReviewNotes = result.Issues
		// 同步填充 Structured（验收关键字段用）
		var fields map[string]any
		if err := json.Unmarshal([]byte(stripJSONFence(s.OCRText)), &fields); err == nil {
			s.Structured = fields
		}
		s.Err = nil
		return s, nil
	}
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

// mimeOf 按扩展名推断图片 MIME（默认 image/png）。
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

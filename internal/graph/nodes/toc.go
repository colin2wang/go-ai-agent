// 本文件实现「扫描版图书目录 → 文字版目录」子功能的节点：
//
//	ocr（逐页 VLM 提取）→ merge（跨页合并校对）→ render（模板渲染）
package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tmc/langchaingo/llms"

	"go-ai-agent/internal/config"
	"go-ai-agent/internal/tmpl"
)

// TOCEntry 一条目录条目。
type TOCEntry struct {
	Level  int    `json:"level"`  // 1=篇/章，2=节，3=小节…
	Number string `json:"number"` // 原始编号文本，如 "第3章" / "2.1"
	Title  string `json:"title"`
	Page   int    `json:"page"` // 0 表示缺失
}

// TOCData 模板渲染的数据上下文（字段对应 templates/book-toc/*.tpl）。
type TOCData struct {
	Title       string
	PageCount   int
	GeneratedAt string
	Template    string
	Entries     []TOCEntry
}

// tocPayload 目录 JSON 的统一结构：{"title":..., "entries":[...]}。
type tocPayload struct {
	Title   string     `json:"title"`
	Entries []TOCEntry `json:"entries"`
}

// TocOCR 逐页 OCR：每张扫描页 → 该页的目录条目 JSON。
// d.Parallel>=2 时多页并发请求（结果仍按页序汇总），provider 并发闸门在 call 层生效；
// 每页完成时通过进度回调上报“第 n/m 页”，供异步任务 SSE/轮询展示。
func TocOCR(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		st := d.stepCfg("ocr")
		pages := s.Pages
		if len(pages) == 0 && s.ImagePath != "" {
			pages = []string{s.ImagePath}
		}
		if len(pages) == 0 {
			s.Err = fmt.Errorf("未收到任何页面图片")
			return s, s.Err
		}

		outs := make([]string, len(pages))
		var (
			mu      sync.Mutex
			wg      sync.WaitGroup
			errOnce sync.Once
			firstEr error
			done    int
		)
		sem := make(chan struct{}, max(d.Parallel, 1))
		for i, p := range pages {
			wg.Add(1)
			go func(i int, p string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				text, err := d.tocOCPage(ctx, st, i, p)
				if err != nil {
					// 保留错误到汇总阶段统一裁决（不中断其余页，已发起的调用不浪费）
					errOnce.Do(func() { firstEr = fmt.Errorf("第 %d 页 OCR 失败: %w", i+1, err) })
					return
				}
				mu.Lock()
				outs[i] = text
				done++
				cur := done
				mu.Unlock()
				d.report("ocr", cur, len(pages), fmt.Sprintf("第 %d 页完成", i+1))
			}(i, p)
		}
		wg.Wait()
		if firstEr != nil {
			s.Err = firstEr
			return s, s.Err
		}

		s.PageOCR = outs
		s.OCRText = strings.Join(outs, "\n\n")
		s.Err = nil
		return s, nil
	}
}

// tocOCPage 单页提取：读图 → 构造多模态消息 → 调用 LLM，返回该页原始输出。
func (d Deps) tocOCPage(ctx context.Context, st config.Step, index int, path string) (string, error) {
	img, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取第 %d 页失败: %w", index+1, err)
	}
	sys, usr := d.stepPrompt("ocr")
	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, sys),
		// langchaingo v0.1.14 会把 BinaryPart 序列化为其私有 {"type":"binary"} 格式，
		// LM Studio 等端点不识别导致图片被静默忽略、模型空回答。
		// 这里直接构造标准 OpenAI image_url（data URL），与 chat 路径的 wire 格式一致。
		{
			// 多模态消息必须显式声明 role，否则 langchaingo 报 "role not supported"
			Role: llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{
				llms.ImageURLContent{URL: imageDataURL(mimeOf(path), img)},
				llms.TextPart(usr),
			},
		},
	}
	text, err := d.call(ctx, "ocr", st.Provider, st.Model, messages, st.Temp(), st.MaxTokens)
	if err != nil {
		return "", err
	}
	// 空回答不立即失败，保留到 merge 阶段回退合并时统一裁决
	if strings.TrimSpace(text) == "" {
		slog.Warn("toc ocr 空回答",
			slog.Int("page", index+1), slog.String("provider", st.Provider), slog.String("model", st.Model))
	} else {
		slog.Info("toc ocr 完成",
			slog.Int("page", index+1),
			slog.Int("answer_chars", len([]rune(text))),
			slog.String("preview", onelinePreview(text)))
	}
	return text, nil
}

// TocMerge 跨页合并：先由 LLM 校对去重排序，未返回合法结果时回退逐页确定性合并。
func TocMerge(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		if len(s.PageOCR) == 0 {
			s.Err = fmt.Errorf("没有可合并的 OCR 结果")
			return s, s.Err
		}

		// 汇总各页原始输出作为合并输入
		var b strings.Builder
		for i, raw := range s.PageOCR {
			fmt.Fprintf(&b, "\n--- 第 %d 页 ---\n%s", i+1, raw)
		}

		st := d.stepCfg("merge")
		sys, _ := d.stepPrompt("merge")
		messages := []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, sys),
			llms.TextParts(llms.ChatMessageTypeHuman, b.String()),
		}
		text, err := d.call(ctx, "merge", st.Provider, st.Model, messages, st.Temp(), st.MaxTokens)
		if err != nil {
			slog.Warn("toc merge LLM 调用失败，将回退逐页合并", slog.String("error", err.Error()))
		} else if strings.TrimSpace(text) == "" {
			slog.Warn("toc merge LLM 空回答，将回退逐页合并",
				slog.String("provider", st.Provider), slog.String("model", st.Model))
		} else {
			slog.Info("toc merge 完成", slog.Int("answer_chars", len([]rune(text))))
		}

		var entries []TOCEntry
		title := ""
		if err == nil {
			entries, title, err = parseTOC(text)
		}
		if err != nil || len(entries) == 0 {
			// 回退：逐页解析后确定性合并（仅去重 + 排序，不做语义修正）
			for _, raw := range s.PageOCR {
				if es, t, perr := parseTOC(raw); perr == nil {
					entries = append(entries, es...)
					if title == "" {
						title = t
					}
				}
			}
			entries = dedupEntries(entries)
			sortEntries(entries)
			if err != nil {
				s.ReviewNotes = append(s.ReviewNotes,
					fmt.Sprintf("合并步骤未返回合法结果（%v），已回退为逐页合并", err))
			}
		}

		if len(entries) == 0 {
			s.Err = fmt.Errorf("未能解析出任何目录条目")
			return s, s.Err
		}
		s.Entries = entries
		s.Title = title
		s.Err = nil
		return s, nil
	}
}

// TocRender 按模板渲染文字版目录（本地执行，不调用 LLM）。
func TocRender(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		name := d.Output.Template
		if name == "" {
			name = "markdown"
		}
		engine := d.Template
		if engine == nil {
			engine = tmpl.New("")
		}
		data := TOCData{
			Title:       strings.TrimSpace(s.Title),
			PageCount:   len(s.Pages),
			GeneratedAt: time.Now().Format("2006-01-02 15:04"),
			Template:    name,
			Entries:     s.Entries,
		}
		if data.Title == "" {
			data.Title = "图书目录"
		}
		out, err := engine.Render("book-toc", name, data)
		if err != nil {
			s.Err = err
			return s, err
		}
		s.FinalReport = out
		s.Structured = map[string]any{
			"title":      data.Title,
			"page_count": data.PageCount,
			"template":   name,
			"count":      len(s.Entries),
			"entries":    s.Entries,
		}
		s.Err = nil
		return s, nil
	}
}

// onelinePreview 将多行文本折叠为单行并截断，用于日志预览。
func onelinePreview(s string) string {
	line := strings.Join(strings.Fields(s), " ")
	if len(line) > 200 {
		line = line[:200] + "..."
	}
	return line
}

// parseTOC 解析目录 JSON：支持 {"title","entries"} 与裸数组两种形式。
func parseTOC(text string) ([]TOCEntry, string, error) {
	raw := strings.TrimSpace(stripJSONFence(text))
	if raw == "" {
		return nil, "", fmt.Errorf("空内容")
	}
	var p tocPayload
	if err := json.Unmarshal([]byte(raw), &p); err == nil && len(p.Entries) > 0 {
		return p.Entries, p.Title, nil
	}
	var arr []TOCEntry
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return arr, "", nil
	}
	return nil, "", fmt.Errorf("输出不是合法目录 JSON")
}

// dedupEntries 按 页码+编号+标题 去重，保留首次出现。
func dedupEntries(in []TOCEntry) []TOCEntry {
	seen := make(map[string]bool, len(in))
	out := make([]TOCEntry, 0, len(in))
	for _, e := range in {
		key := fmt.Sprintf("%d|%s|%s", e.Page, strings.TrimSpace(e.Number), strings.TrimSpace(e.Title))
		if key == "0||" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

// sortEntries 按页码升序排序；page=0（缺页码）排在末尾，保持原有相对顺序。
func sortEntries(in []TOCEntry) {
	sort.SliceStable(in, func(i, j int) bool {
		pi, pj := in[i].Page, in[j].Page
		if pi <= 0 {
			pi = math.MaxInt
		}
		if pj <= 0 {
			pj = math.MaxInt
		}
		return pi < pj
	})
}

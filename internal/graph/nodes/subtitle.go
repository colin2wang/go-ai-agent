// 本文件实现「SRT 字幕 → 目标语言 SRT」子功能的节点：
//
//	parse（本地解析 SRT）→ translate（分批 LLM 翻译）→ render（本地重组，无 LLM）
//
// 只有 translate 会调用模型；parse / render 都是确定性本地处理，因此序号、
// 时间轴和空行结构原样保留，模型只负责替换对白文本。
package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tmc/langchaingo/llms"

	"go-ai-agent/internal/config"
)

// defaultSubtitleBatchSize 未声明 workflow.batch-size 时每批的字幕条数。
const defaultSubtitleBatchSize = 20

// defaultTargetLang Params 未下发目标语言时的兜底（正常路径由 yml 的
// input.target-language.default 经 API 层填入，不会走到这里）。
const defaultTargetLang = "简体中文"

// SRTBlock 一个字幕块：序号与时间轴行原样保留，只替换 Lines。
type SRTBlock struct {
	Index string   // 原始序号文本
	Time  string   // 原始时间轴行（含 -->）
	Lines []string // 字幕文本行（多行字幕为多行）
}

// SubtitleParse 读取上传的 SRT 文件并解析为字幕块（本地执行，不调用 LLM）。
func SubtitleParse(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		path := s.ImagePath
		if path == "" && len(s.Pages) > 0 {
			path = s.Pages[0]
		}
		if path == "" {
			s.Err = fmt.Errorf("未收到字幕文件")
			return s, s.Err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			s.Err = fmt.Errorf("读取字幕文件失败: %w", err)
			return s, s.Err
		}
		blocks, err := parseSRT(string(data))
		if err != nil {
			s.Err = err
			return s, s.Err
		}
		s.SRTBlocks = blocks
		s.SRTSource = string(data)
		s.TargetLang = subtitleTargetLang(d)
		s.Err = nil
		slog.Info("字幕解析完成",
			slog.Int("blocks", len(blocks)), slog.String("target_lang", s.TargetLang))
		return s, nil
	}
}

// SubtitleTranslate 按 batch-size 切分字幕、并发送模型翻译，逐批上报进度。
// 译文格式不符或条数不对时，该批降级为逐条重译；调用失败则保留原文并在
// ReviewNotes 中提示，保证最终输出始终是条数、时间轴对齐的完整 SRT。
func SubtitleTranslate(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		if len(s.SRTBlocks) == 0 {
			s.Err = fmt.Errorf("没有可翻译的字幕块（parse 步骤未产出结果）")
			return s, s.Err
		}
		st := d.stepCfg("translate")

		// 空文本不送模型，但块本身保留（时间轴仍需出现在成品里）。
		texts := make([]string, 0, len(s.SRTBlocks))
		blockIdx := make([]int, 0, len(s.SRTBlocks))
		for i, b := range s.SRTBlocks {
			joined := strings.Join(b.Lines, "\n")
			if strings.TrimSpace(joined) == "" {
				continue
			}
			texts = append(texts, joined)
			blockIdx = append(blockIdx, i)
		}
		if len(texts) == 0 {
			s.Err = fmt.Errorf("字幕文件中没有可翻译的文本")
			return s, s.Err
		}

		size := subtitleBatchSize(d)
		batches := splitBatches(texts, size)
		results := append([]string(nil), texts...) // 默认原文，失败时原样保留

		var (
			mu    sync.Mutex
			wg    sync.WaitGroup
			notes []string
			done  int
		)
		sem := make(chan struct{}, max(d.Parallel, 1))
		d.report("translate", 0, len(batches), fmt.Sprintf("共 %d 条字幕 / %d 批", len(texts), len(batches)))

		for bi, batch := range batches {
			wg.Add(1)
			go func(bi int, batch []string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				start := bi * size
				got, note := d.translateSRTBatch(ctx, st, batch, s.TargetLang)
				mu.Lock()
				for j, line := range got {
					results[start+j] = line
				}
				if note != "" {
					notes = append(notes, fmt.Sprintf("第 %d 批：%s", bi+1, note))
				}
				done++
				cur := done
				mu.Unlock()
				d.report("translate", cur, len(batches), fmt.Sprintf("第 %d/%d 批完成", bi+1, len(batches)))
			}(bi, batch)
		}
		wg.Wait()

		translated := make([]SRTBlock, len(s.SRTBlocks))
		copy(translated, s.SRTBlocks)
		for k, idx := range blockIdx {
			translated[idx].Lines = splitSRTLines(results[k])
		}
		s.SRTBlocks = translated
		s.ReviewNotes = append(s.ReviewNotes, notes...)
		s.Err = nil
		return s, nil
	}
}

// SubtitleRender 把译文重组为完整 SRT（本地执行，不调用 LLM）。
// FinalReport 供前端「下载」按钮保存为 .srt；Translation 供「翻译」tab 展示；
// OCRText 复用为原始字幕文本，便于与译文对照。
func SubtitleRender(d Deps) func(ctx context.Context, s State) (State, error) {
	return func(ctx context.Context, s State) (State, error) {
		out := renderSRT(s.SRTBlocks)
		if strings.TrimSpace(out) == "" {
			s.Err = fmt.Errorf("生成的字幕内容为空")
			return s, s.Err
		}
		s.FinalReport = out
		s.Translation = out
		s.OCRText = s.SRTSource
		s.Structured = map[string]any{
			"target_language": s.TargetLang,
			"subtitle_count":  len(s.SRTBlocks),
			"batch_size":      subtitleBatchSize(d),
		}
		s.Err = nil
		return s, nil
	}
}

// translateSRTBatch 翻译一批字幕，返回值长度始终等于入参长度。
// 第二返回值为降级提示（正常路径为空）。
func (d Deps) translateSRTBatch(ctx context.Context, st config.Step, lines []string, lang string) ([]string, string) {
	out := append([]string(nil), lines...)
	raw, err := d.callSRTBatch(ctx, st, lines, lang)
	if err != nil {
		// 端点/网络错误不做逐条降级（避免把一次故障放大成 N 次失败请求）。
		return out, fmt.Sprintf("模型调用失败（%v），本批保留原文", err)
	}
	parsed, err := parseTranslatedLines(raw, len(lines))
	if err == nil {
		return parsed, ""
	}

	// 输出格式或条数不符（常见于批次过长被 max-tokens 截断）：逐条重译，
	// 单条输入更短、更容易满足格式约束；仍失败的条目保留原文。
	failed := 0
	for i, src := range lines {
		oneRaw, e := d.callSRTBatch(ctx, st, []string{src}, lang)
		if e != nil {
			failed++
			continue
		}
		one, e := parseTranslatedLines(oneRaw, 1)
		if e != nil {
			failed++
			continue
		}
		out[i] = one[0]
	}
	return out, fmt.Sprintf("译文格式不符合要求（%v），已逐条重试；%d 条保留原文", err, failed)
}

// callSRTBatch 组装一次 LLM 调用：目标语言 + 提示词 + JSON 字符串数组输入。
// 用 json.Marshal 编码可保证字幕里的引号、换行、格式标记不会被破坏。
func (d Deps) callSRTBatch(ctx context.Context, st config.Step, lines []string, lang string) (string, error) {
	payload, err := json.Marshal(lines)
	if err != nil {
		return "", fmt.Errorf("编码字幕批次失败: %w", err)
	}
	sys, usr := d.stepPrompt("translate")
	var b strings.Builder
	fmt.Fprintf(&b, "目标语言：%s\n", lang)
	if v := strings.TrimSpace(usr); v != "" {
		b.WriteString(v)
		b.WriteByte('\n')
	}
	b.WriteString("待翻译字幕（JSON 字符串数组，元素个数与顺序必须保持一致）：\n")
	b.WriteString(string(payload))

	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, sys),
		llms.TextParts(llms.ChatMessageTypeHuman, b.String()),
	}
	return d.call(ctx, "translate", st.Provider, st.Model, messages, st.Temp(), st.MaxTokens)
}

// parseTranslatedLines 解析模型返回的 JSON 字符串数组，容忍代码围栏与前后解释文字。
func parseTranslatedLines(text string, want int) ([]string, error) {
	raw := strings.TrimSpace(text)
	for _, fence := range []string{"```json", "```JSON", "```"} {
		if strings.HasPrefix(raw, fence) {
			raw = strings.TrimPrefix(raw, fence)
			break
		}
	}
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "```"))
	if start := strings.Index(raw, "["); start >= 0 {
		if end := strings.LastIndex(raw, "]"); end > start {
			raw = raw[start : end+1]
		}
	}
	if raw == "" {
		return nil, fmt.Errorf("译文为空")
	}
	var lines []string
	if err := json.Unmarshal([]byte(raw), &lines); err != nil {
		return nil, fmt.Errorf("译文不是合法 JSON 字符串数组: %w", err)
	}
	if len(lines) != want {
		return nil, fmt.Errorf("译文条数 %d 与原文 %d 不一致", len(lines), want)
	}
	return lines, nil
}

// parseSRT 把 SRT 文本解析成字幕块：去 BOM、统一换行、按空行分块。
// 序号与时间轴行保持原文；缺少序号的块按出现顺序自动编号。
func parseSRT(raw string) ([]SRTBlock, error) {
	text := strings.ReplaceAll(strings.TrimPrefix(raw, "\ufeff"), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("字幕文件不是有效的 UTF-8 文本，请先转换为 UTF-8 编码的 SRT")
	}
	var blocks []SRTBlock
	for _, chunk := range splitSRTChunks(text) {
		b, ok := parseSRTChunk(chunk)
		if !ok {
			continue // 跳过文件头等非字幕块（如 WEBVTT 头）
		}
		if b.Index == "" {
			b.Index = strconv.Itoa(len(blocks) + 1)
		}
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("未解析到任何字幕块，请确认上传的是标准 SRT 文件（序号 / 时间轴 / 对白文本）")
	}
	return blocks, nil
}

// splitSRTChunks 以一个或多个空行为界切分字幕块，块内行不含空行。
func splitSRTChunks(text string) [][]string {
	var out [][]string
	var cur []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				out = append(out, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// parseSRTChunk 以含 "-->" 的行为时间轴：其前一行视作序号，其后全部为对白文本。
// 采用宽松定位而非固定行号，可容忍序号缺失、序号与时间轴之间多余空行等变体。
func parseSRTChunk(lines []string) (SRTBlock, bool) {
	ti := -1
	for i, line := range lines {
		if strings.Contains(line, "-->") {
			ti = i
			break
		}
	}
	if ti < 0 {
		return SRTBlock{}, false
	}
	b := SRTBlock{
		Index: strings.TrimSpace(lines[0]),
		Time:  strings.TrimSpace(lines[ti]),
	}
	// 只有第一行是纯数字时才当作序号，否则留空由 parseSRT 自动编号。
	if _, err := strconv.Atoi(b.Index); err != nil {
		b.Index = ""
	}
	for _, line := range lines[ti+1:] {
		b.Lines = append(b.Lines, strings.TrimSpace(line))
	}
	return b, true
}

// renderSRT 按标准 SRT 格式重组：序号 / 时间轴 / 文本行 / 空行。
func renderSRT(blocks []SRTBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		b.WriteString(blk.Index)
		b.WriteByte('\n')
		b.WriteString(blk.Time)
		b.WriteByte('\n')
		for _, line := range blk.Lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// splitBatches 按固定条数切分（最后一批可能不足 size）。
func splitBatches(lines []string, size int) [][]string {
	if size <= 0 {
		size = defaultSubtitleBatchSize
	}
	out := make([][]string, 0, (len(lines)+size-1)/size)
	for i := 0; i < len(lines); i += size {
		out = append(out, lines[i:min(i+size, len(lines))])
	}
	return out
}

// splitSRTLines 把一条译文按 \n 还原成字幕行，去掉尾部多余空行。
func splitSRTLines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimRight(p, "\r"))
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

// subtitleBatchSize 决定单批送模型的字幕条数：优先取表单参数 batch_size
// （1-200；0 或非法值忽略），其次 yml 的 workflow.batch-size，最后默认值。
func subtitleBatchSize(d Deps) int {
	if raw := strings.TrimSpace(d.Params[config.BatchSizeField]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			return n
		}
	}
	if d.BatchSize > 0 {
		return d.BatchSize
	}
	return defaultSubtitleBatchSize
}

func subtitleTargetLang(d Deps) string {
	if v := strings.TrimSpace(d.Params[config.TargetLangField]); v != "" {
		return v
	}
	return defaultTargetLang
}

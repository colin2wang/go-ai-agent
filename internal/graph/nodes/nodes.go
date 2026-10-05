// Package nodes 实现各业务节点：每个节点独立声明 provider+model，互不影响。
package nodes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tmc/langchaingo/llms"

	"go-ai-agent/internal/config"
	"go-ai-agent/internal/llm"
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
	// RetryMax / RetryBackoffMS 单次调用内的重试与指数退避（限流/5xx 才重试）。
	RetryMax       int
	RetryBackoffMS int
	// Workflow 工作流类型（来自子功能 workflow.type），供编排层选择图。
	Workflow string
	// Parallel 逐页并发度（>=2 时多页 OCR 并发执行，结果仍按页序汇总）。
	Parallel int
	// CacheEnabled 本次工作流的步骤是否默认参与 LLM 结果缓存。
	CacheEnabled bool
	// NoCacheSteps 显式声明 cache: false 的步骤名集合（在 CacheEnabled=true 时排除这些步骤）。
	NoCacheSteps map[string]bool
	// Progress 进度回调（异步任务向 SSE 推送“第 n/m 页”），可为 nil。
	Progress func(step string, done, total int, detail string)
	// Template 模板引擎（本地渲染步骤使用，可为 nil）。
	Template *tmpl.Engine
	// Output 输出声明（模板名 / 外部模板目录 / 扩展名）。
	Output config.Output
}

// report 上报一次步骤进度（无回调时 no-op）。
func (d Deps) report(step string, done, total int, detail string) {
	if d.Progress == nil {
		return
	}
	if detail == "" {
		detail = fmt.Sprintf("%s %d/%d", step, done, total)
	} else {
		detail = fmt.Sprintf("%s %d/%d：%s", step, done, total, detail)
	}
	d.Progress(step, done, total, detail)
}

// stepCfg 返回节点对应的步骤配置（未单独声明则回退 Default）。
func (d Deps) stepCfg(name string) config.Step {
	return d.Steps[name].WithDefaults(d.Default)
}

// call 封装一次 LLM 调用：取客户端 → 组装选项 → GenerateContent，返回回答文本。
// 调用前后受 provider 并发闸门约束（避免本地端点被并发打爆），失败时按
// 退避策略重试：仅对限流/5xx/网络类错误重试，4xx 参数错误直接返回。
func (d Deps) call(ctx context.Context, stepName, provider, model string,
	messages []llms.MessageContent, temperature float64, maxTokens int) (string, error) {

	chat, err := d.Factory.Get(provider, model)
	if err != nil {
		return "", err
	}
	if d.cacheForStep(stepName) {
		ctx = llm.WithCacheEnabled(ctx, true)
	}
	// 重试循环与具体调用方式解耦：fn 可替换为任意序列（便于单测覆盖退避行为）。
	return retryLoop(ctx, retryPolicy{Max: d.RetryMax, BackoffMS: d.RetryBackoffMS}, stepName,
		func() (string, error) {
			return d.callOnce(ctx, chat, stepName, messages, temperature, maxTokens)
		})
}

// retryPolicy 单次调用内的重试策略（Max 为「额外」尝试次数，不含首次）。
type retryPolicy struct {
	Max       int // <=0 归一为 0（不重试）
	BackoffMS int // 退避基数（毫秒，<=0 = 不退避立即重试）
}

// retryLoop 按策略执行 fn：成功即返回；失败且可重试则退避后再次尝试，
// 最多额外 Max 次；不可重试错误、次数耗尽或 ctx 取消时立即返回最后错误。
// 与具体调用方式解耦，便于用可控的错误序列验证退避行为。
func retryLoop(ctx context.Context, pol retryPolicy, stepName string, fn func() (string, error)) (string, error) {
	if pol.Max < 0 {
		pol.Max = 0
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		text, err := fn()
		if err == nil {
			return text, nil
		}
		lastErr = err
		if attempt >= pol.Max || !retryableErr(err) {
			return "", lastErr
		}
		slog.Warn("步骤调用失败，准备重试",
			slog.String("step", stepName), slog.Int("attempt", attempt+1),
			slog.Int("max", pol.Max), slog.String("error", err.Error()))
		if !sleepBackoff(ctx, pol.BackoffMS, attempt, err) {
			return "", lastErr // 等待期间 ctx 取消，不再重试
		}
	}
}

// callOnce 单次实际调用（含 provider 并发闸门）。
func (d Deps) callOnce(ctx context.Context, chat llm.Model, stepName string,
	messages []llms.MessageContent, temperature float64, maxTokens int) (string, error) {

	release, err := d.Factory.Acquire(ctx, d.stepProvider(stepName))
	if err != nil {
		return "", fmt.Errorf("step %s 等待并发额度: %w", stepName, err)
	}
	defer release()

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

// cacheForStep 该步骤是否参与结果缓存：全局开启且未被步骤级 cache:false 排除。
func (d Deps) cacheForStep(stepName string) bool {
	return d.CacheEnabled && !d.NoCacheSteps[stepName]
}

// stepProvider 返回该步骤最终生效的 provider 名（限流闸门按它分组）。
func (d Deps) stepProvider(stepName string) string {
	return d.stepCfg(stepName).Provider
}

// httpStatusRe 从错误文本中提取端点返回的 HTTP 状态码（langchaingo 不包装状态码，
// 但错误消息普遍形如 "error, status code: 429"，据此区分限流/服务端/参数错误）。
var httpStatusRe = regexp.MustCompile(`status code:?\s*(\d{3})`)

// retryableErr 判断是否值得重试：网络/超时与 408/429/5xx 可重试，其余 4xx 立即失败。
func retryableErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false // 调用方主动取消
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	m := strings.ToLower(err.Error())
	if code := errorCode(m); code > 0 {
		return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
	}
	// 无状态码的连接层错误（拒绝连接/DNS/读超时）对本地端点重启场景值得再试
	for _, hint := range []string{"connection refused", "connection reset", "eof", "timeout", "no such host", "temporarily unavailable"} {
		if strings.Contains(m, hint) {
			return true
		}
	}
	return false
}

// errorCode 提取错误文本中的 HTTP 状态码，不存在返回 0。
func errorCode(m string) int {
	g := httpStatusRe.FindStringSubmatch(m)
	if len(g) < 2 {
		return 0
	}
	n, err := strconv.Atoi(g[1])
	if err != nil {
		return 0
	}
	return n
}

// maxBackoff 指数退避的等待上限（避免极端 base 把工作流拖死）。
const maxBackoff = 30 * time.Second

// maxRetryAfter 遵循端点 Retry-After 时的等待上限。
const maxRetryAfter = 60 * time.Second

// retryAfterRe 从错误消息中提取 Retry-After 提示（秒或毫秒）。
// langchaingo 不透出响应头，但 OpenAI 兼容端点常把限流提示带在错误正文里
// （形如 "Retry-After: 30"、'{"error":{"message":"...retry after 1500ms"}}'），
// 此时应优先遵循端点指示，而不是按固定指数退避过早重试加剧限流。
var retryAfterRe = regexp.MustCompile(`(?i)retry[- ]?after\D{0,8}(\d+)\s*(ms|s)?`)

// retryAfterFromErr 解析端点给出的重试等待时长；解析不到返回 (0, false)。
func retryAfterFromErr(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	g := retryAfterRe.FindStringSubmatch(err.Error())
	if len(g) < 2 {
		return 0, false
	}
	n, convErr := strconv.Atoi(g[1])
	if convErr != nil || n <= 0 {
		return 0, false
	}
	d := time.Duration(n) * time.Second
	if len(g) > 2 && strings.EqualFold(g[2], "ms") {
		d = time.Duration(n) * time.Millisecond
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d, true
}

// backoffDelay 计算本次重试的等待时长：端点给出 Retry-After 时优先遵循，
// 否则按指数退避（base × 2^attempt，上限 maxBackoff）。base<=0 时为 0（立即重试）。
func backoffDelay(baseMS, attempt int, err error) time.Duration {
	if d, ok := retryAfterFromErr(err); ok {
		return d
	}
	if baseMS <= 0 {
		return 0
	}
	d := time.Duration(baseMS) * time.Millisecond * time.Duration(1<<min(attempt, 5))
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// sleepBackoff 等待一次重试间隔（带 ±25% 抖动，避免并发页同时重试踩同一坑）；
// ctx 提前取消时返回 false。等待时长为 0 时不挂起（保持旧行为：立即重试）。
func sleepBackoff(ctx context.Context, baseMS, attempt int, err error) bool {
	d := backoffDelay(baseMS, attempt, err)
	if d <= 0 {
		return ctx.Err() == nil
	}
	d += time.Duration(rand.Int63n(int64(d)/4+1)) - d/8
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// OCR 视觉 OCR 节点：工资单图片（一张或多张）→ 结构化 JSON。
// 全部图片放进同一条多模态消息，模型输出合并后的单份结果，
// 下游步骤无需感知图片数量（与逐页并发的 OCRPage 区分）。
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
		d.report("ocr", 0, 1, fmt.Sprintf("%d 张图片", len(images)))
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
		d.report("ocr", 1, 1, "")
		return s, nil
	}
}

// stepPrompt 返回步骤的系统/用户提示词（直接来自 yml 的 system-prompt/user-prompt）。
// 提示词的唯一定义位置是各子功能的 features/*.yml，不在此处硬编码任何文本。
func (d Deps) stepPrompt(name string) (system, user string) {
	st := d.stepCfg(name)
	return st.SystemPrompt, st.UserPrompt
}

// maxImageLongEdge 发送前将超长边图片等比缩放到此像素内。
// 扫描版图书目录/工资单常为高分辨率大图，本地 VLM（如 LM Studio 上的
// Qwen3-VL）对超大图片会静默丢弃、直接返回空回答（表现为 choices=1 但
// content 为空、耗时极短）。先验缩放到合理分辨率可显著降低视觉 token 量、
// 规避该问题，同时保持文本清晰可读。
const maxImageLongEdge = 1600

// maxDecodePixels 允许本地解码的最大像素数（防解码炸弹）。
// 常规扫描件远低于此：A4 300dpi ≈ 8.7MP、手机照片约 12MP，留足余量。
const maxDecodePixels = 50_000_000

// decodeDimensions 只读取图片头取得宽高（不解码像素数据，开销恒定且极小）。
func decodeDimensions(data []byte) (w, h int, ok bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

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
		// 解码前先看头部声明的宽高：恶意/异常图片可以极小体积声明超大尺寸
		// （解码炸弹），一次 image.Decode 就会按 宽×高×4 字节吃满内存。
		// 超限时不解码、不缩放，原样转发给端点（端点处理不了会正常报错），
		// 而不是让进程 OOM。
		if w, h, ok := decodeDimensions(data); ok && int64(w)*int64(h) > maxDecodePixels {
			slog.Warn("图片像素数超限，跳过本地缩放直接发送",
				slog.Int("width", w), slog.Int("height", h),
				slog.Int64("pixels", int64(w)*int64(h)), slog.Int("limit", maxDecodePixels))
			break
		}
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

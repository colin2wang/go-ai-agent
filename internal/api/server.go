// Package api 提供 REST API：
//   - /api/features    子功能清单（前端左侧菜单）
//   - /api/overview    概览指标（调用统计聚合）
//   - /api/features/{id}/run  执行某子功能工作流（上传文件）
//
// Swagger 注解经 swaggo 生成 OpenAPI 文档（/swagger/index.html）。
package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go-ai-agent/internal/config"
	"go-ai-agent/internal/graph"
	"go-ai-agent/internal/graph/nodes"
	"go-ai-agent/internal/llm"
	"go-ai-agent/internal/stats"
	"go-ai-agent/internal/tmpl"
	"go-ai-agent/pkg/iolog"
)

// Server HTTP 服务，内部持有已初始化的流水线依赖。
type Server struct {
	cfg      *config.Config
	factory  *llm.Factory
	recorder *stats.Recorder
	sink     *llm.StatsSink
	tmpDir   string

	features  []config.Feature
	pipelines map[string]nodes.Deps // featureID → 工作流依赖

	capsCache *modelCapsCache // 模型能力探测缓存（provider|model → 结果，启动时从 SQLite 预热）
	probeLock sync.Mutex      // 串行化能力探测：同 key 并发请求只探测一次

	routeOnce sync.Once // 首次请求时构建路由树（含鉴权包装）
	router    http.Handler

	// Version 由 main 注入（ldflags 编译期版本）。
	Version string

	mu         sync.Mutex
	jobs       map[string]*JobResult // jobID → 结果（未启用 persist-jobs 时的内存态回退）
	jobOrder   []string              // jobs 的插入顺序，用于按 maxMemJobs 淘汰最旧任务
	numWorking atomic.Int32          // 执行中的异步任务数（graceful shutdown 排空用）
	done       chan struct{}         // 关闭以停止后台清理协程
}

// JobResult 一次子功能任务的执行结果（异步模式下兼作进度快照）。
type JobResult struct {
	JobID       string         `json:"job_id"`
	FeatureID   string         `json:"feature_id,omitempty"`
	Status      string         `json:"status"`             // pending | processing | running | done | failed
	Progress    int            `json:"progress,omitempty"` // 已完成步数（逐页 OCR 时为已完成页数）
	Total       int            `json:"total,omitempty"`
	Detail      string         `json:"detail,omitempty"` // 进度描述（如“ocr 2/5”）
	FinalReport string         `json:"final_report,omitempty"`
	Structured  map[string]any `json:"structured,omitempty"`
	OCRText     string         `json:"ocr_text,omitempty"`
	Translation string         `json:"translation,omitempty"`
	ReviewNotes []string       `json:"review_notes,omitempty"`
	Error       string         `json:"error,omitempty"`
	DurationMS  int64          `json:"duration_ms"`
	CreatedAt   string         `json:"created_at,omitempty"`
	UpdatedAt   string         `json:"updated_at,omitempty"`
}

// FeatureInfo 子功能对外描述（前端菜单 / 上传控件 / 流程链）。
type FeatureInfo struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Icon        string        `json:"icon"`
	Order       int           `json:"order"`
	Kind        string        `json:"kind"` // workflow（默认，上传执行）| chat（对话）
	Pipeline    []string      `json:"pipeline"`
	Steps       []string      `json:"steps"`
	Input       FeatureInput  `json:"input"`
	Output      FeatureOutput `json:"output"`
	Chat        FeatureChat   `json:"chat"`
	Async       bool          `json:"async"` // true = 上传后立即返回 job_id，前端轮询获取进度与结果
}

// FeatureInput 输入声明（前端据此渲染上传控件，max_* 为服务端强校验上限）。
type FeatureInput struct {
	Kind       string `json:"kind"`
	Multiple   bool   `json:"multiple"`
	Accept     string `json:"accept"`
	Hint       string `json:"hint"`
	MaxFiles   int    `json:"max_files"`
	MaxFileMB  int    `json:"max_file_mb"`
	MaxTotalMB int    `json:"max_total_mb"`
}

// FeatureOutput 输出声明（模板名 / 外部模板目录 / 下载扩展名）。
type FeatureOutput struct {
	Template    string `json:"template"`
	TemplateDir string `json:"template_dir"`
	Extension   string `json:"extension"`
}

// FeatureChat 对话类子功能的前端可见配置（不含任何密钥）。
type FeatureChat struct {
	DefaultProvider string `json:"default_provider"`
	StreamDefault   bool   `json:"stream_default"`
	MaxHistory      int    `json:"max_history"`
	MaxImages       int    `json:"max_images"`   // 单条消息最多附带图片数（仅视觉模型）
	MaxImageMB      int    `json:"max_image_mb"` // 单张图片体积上限 MB
}

// ProviderInfo AI 端点对外描述（不含 api-key）。
type ProviderInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	Default bool   `json:"default"`
}

// New 初始化 Server（工厂/统计/临时目录），为每个子功能预构建工作流依赖，
// 并完成结果缓存、provider 并发闸门、能力探测持久化与后台清理的接线。
func New(cfg *config.Config) (*Server, error) {
	var recorder *stats.Recorder
	var sink *llm.StatsSink
	if cfg.Storage.Enabled {
		var err error
		recorder, err = stats.Open(cfg.Storage.DBPath, cfg.Storage.RetentionDays)
		if err != nil {
			return nil, fmt.Errorf("init storage: %w", err)
		}
		sink = llm.NewStatsSink(recorder, cfg.Storage.QueueCapacity)
	}
	factory, err := llm.NewFactory(cfg.LLM.Providers, sink, iolog.New(cfg.LLM.IoLog).Hook())
	if err != nil {
		return nil, fmt.Errorf("init llm factory: %w", err)
	}
	if cfg.LLM.IoLog.Debug {
		factory.EnableWireDump() // HTTP 层抓包，排查端点交互问题
	}

	// LLM 结果缓存：复用统计库的 llm_cache 表（未启用时为 no-op）
	if cfg.Cache.Enabled && recorder != nil {
		factory.SetCache(llm.NewCache(stats.NewCacheStore(recorder), llm.CacheConfig{
			Enabled:   true,
			TTL:       cfg.Cache.TTL(),
			KeepOnErr: cfg.Cache.KeepOnErr,
		}))
		slog.Info("LLM 结果缓存已启用", slog.String("ttl", cfg.Cache.TTL().String()),
			slog.Int("max_rows", cfg.Cache.CacheMaxRows()))
	}

	tmpDir, err := os.MkdirTemp("", "harness-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create tmp dir: %w", err)
	}

	// 每个子功能一套步骤配置；未声明字段回退 ai.yml 全局默认
	s := &Server{
		cfg:       cfg,
		factory:   factory,
		recorder:  recorder,
		sink:      sink,
		tmpDir:    tmpDir,
		features:  cfg.Resolved,
		pipelines: make(map[string]nodes.Deps, len(cfg.Resolved)),
		capsCache: newModelCapsCache(),
		jobs:      map[string]*JobResult{},
		done:      make(chan struct{}),
	}
	maxConc := 0
	for _, f := range cfg.Resolved {
		s.pipelines[f.ID] = s.buildDeps(f, nil)
		if c := f.Workflow.ProviderConcurrency(); c > maxConc {
			maxConc = c
		}
	}
	// 全局闸门取各功能最大值（多功能共用同一批 provider 时互不串扰）
	factory.SetProviderConcurrency(maxConc)

	// 能力探测结果预热：重启后免重测（仅加载 TTL 未过期的行）
	if recorder != nil {
		if minMS := s.capsMinProbedMS(); minMS > 0 {
			if n := recorder.WarmCaps(minMS, s.capsCache.warm); n > 0 {
				slog.Info("模型能力探测缓存已从 SQLite 预热", slog.Int("rows", n))
			}
		}
		// 上次进程退出时仍在执行的任务标记为中断，避免前端永久轮询
		s.failInterruptedJobs()
	}
	go s.pruneLoop()
	return s, nil
}

// buildDeps 为某子功能构造工作流依赖；progress 非 nil 时用于异步任务进度上报
// （每次执行单独构造一份 Deps，避免并发任务间串扰）。
func (s *Server) buildDeps(f config.Feature, progress func(step string, done, total int, detail string)) nodes.Deps {
	globalCache := s.cfg.Cache.Enabled
	noCache := map[string]bool{}
	for name, st := range f.Steps {
		if !st.CacheEnabled(globalCache) {
			noCache[name] = true
		}
	}
	return nodes.Deps{
		Factory: s.factory,
		Steps:   f.Steps,
		Default: config.Step{
			Provider:    s.cfg.LLM.DefaultProvider,
			Model:       envOr("HARNESS_DEFAULT_MODEL", "qwen/qwen3-vl-8b"),
			Temperature: &s.cfg.LLM.Temperature,
			MaxTokens:   s.cfg.LLM.MaxTokens,
		},
		Stream:         s.cfg.LLM.StreamOutput,
		MaxRetries:     f.Workflow.MaxRetries,
		RetryMax:       f.Workflow.CallRetryMax(),
		RetryBackoffMS: f.Workflow.RetryBackoffMS,
		Parallel:       f.Workflow.Parallel(),
		CacheEnabled:   globalCache,
		NoCacheSteps:   noCache,
		Workflow:       f.Workflow.Type,
		Progress:       progress,
		// 模板引擎：内置模板 + 可选的外部目录覆盖
		Template: tmpl.New(f.Output.TemplateDir),
		Output:   f.Output,
	}
}

// depsFor 返回子功能默认的同步执行依赖（无进度回调）。
func (s *Server) depsFor(featureID string) (nodes.Deps, bool) {
	f, ok := s.cfg.Feature(featureID)
	if !ok {
		if d, ok := s.pipelines[featureID]; ok { // 兼容仅有 pipeline 的场景
			return d, true
		}
		return nodes.Deps{}, false
	}
	return s.buildDeps(f, nil), true
}

// asyncRun 判断某子功能的 run 是否走异步任务模式（配置默认 + 请求级 async=1/0 覆盖）。
func (s *Server) asyncRun(featureID string, r *http.Request) bool {
	switch r.URL.Query().Get("async") {
	case "1", "true":
		return s.recorder.Jobs() != nil
	case "0", "false":
		return false
	}
	return s.cfg.Storage.PersistJobs && s.recorder.Jobs() != nil
}

// jobsStore 返回异步任务仓储（统计库未启用时为 nil，各方法对 nil 接收者安全降级）。
func (s *Server) jobsStore() *stats.JobStore {
	if !s.cfg.Storage.PersistJobs {
		return nil // 未显式开启时保持旧行为（同步 + 内存态任务）
	}
	return s.recorder.Jobs()
}

// capsMinProbedMS 能力预热的时间下限（仅加载 TTL 未过期行）；0 = 不过滤。
func (s *Server) capsMinProbedMS() int64 {
	ttl := time.Duration(s.cfg.Storage.CapsTTL()) * time.Hour
	return time.Now().Add(-ttl).UnixMilli()
}

// failInterruptedJobs 启动期把上次残留的 pending/running 任务标为中断。
func (s *Server) failInterruptedJobs() {
	store := s.recorder.Jobs()
	if store == nil {
		return
	}
	n, err := store.FailInterrupted("服务重启，任务中断")
	if err != nil {
		slog.Warn("标记中断任务失败", slog.String("error", err.Error()))
	} else if n > 0 {
		slog.Info("已将上次未完成的任务标记为中断", slog.Int64("rows", n))
	}
}

// pruneLoop 后台周期清理：过期能力探测、超过保留期的任务行、缓存过期与容量淘汰。
func (s *Server) pruneLoop() {
	interval := s.cfg.Storage.PruneInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.pruneOnce()
		}
	}
}

func (s *Server) pruneOnce() {
	if s.recorder == nil {
		return
	}
	ttl := time.Duration(s.cfg.Storage.CapsTTL()) * time.Hour
	if n, err := s.recorder.DropExpiredCaps(ttl); err != nil {
		slog.Warn("清理过期能力探测失败", slog.String("error", err.Error()))
	} else if n > 0 {
		slog.Info("已清理过期能力探测缓存", slog.Int64("rows", n))
	}
	if store := s.recorder.Jobs(); store != nil {
		if err := store.Prune(s.cfg.Storage.RetentionDays); err != nil {
			slog.Warn("清理过期任务失败", slog.String("error", err.Error()))
		}
	}
	if s.cfg.Cache.Enabled {
		if dropped, trimmed, err := s.recorder.PruneCache(s.cfg.Cache.CacheMaxRows()); err != nil {
			slog.Warn("清理 LLM 结果缓存失败", slog.String("error", err.Error()))
		} else if dropped+trimmed > 0 {
			slog.Info("已清理 LLM 结果缓存",
				slog.Int64("expired", dropped), slog.Int64("trimmed", trimmed))
		}
	}
}

// Close 释放资源：停止后台协程，并短暂等待执行中的异步任务落库。
func (s *Server) Close() {
	if s.done != nil {
		close(s.done)
	}
	// 优雅停机：给运行中的异步任务一个收尾窗口（超时后放弃，结果丢失可接受）
	deadline := time.After(5 * time.Second)
	for s.numWorking.Load() > 0 {
		select {
		case <-deadline:
			slog.Warn("仍有未完成的异步任务，放弃等待", slog.Int("working", int(s.numWorking.Load())))
			goto drained
		default:
		}
		time.Sleep(200 * time.Millisecond)
	}
drained:
	if s.sink != nil {
		s.sink.Close()
	}
	if s.recorder != nil {
		_ = s.recorder.Close()
	}
	_ = os.RemoveAll(s.tmpDir)
}

// respond 统一 JSON 输出。
func respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError 统一错误响应体。
type apiError struct {
	Error string `json:"error" example:"error message"`
}

func fail(w http.ResponseWriter, code int, msg string) {
	respond(w, code, apiError{Error: msg})
}

// saveUploads 保存上传文件到临时目录，返回落盘路径列表（按上传顺序）。
// 多页场景用 files 字段（可多个），单文件场景用 file 字段，两者兼容。
// in 携带服务端强校验上限（文件数/单文件体积/总体积）。
func (s *Server) saveUploads(r *http.Request, in config.Input) ([]string, error) {
	if r.MultipartForm == nil {
		return nil, fmt.Errorf("缺少上传文件")
	}
	if headers := r.MultipartForm.File["files"]; len(headers) > 0 {
		if err := checkUploadLimits(in, headers); err != nil {
			return nil, err
		}
		paths := make([]string, 0, len(headers))
		for _, h := range headers {
			p, err := s.saveFileHeader(h)
			if err != nil {
				return nil, err
			}
			paths = append(paths, p)
		}
		return paths, nil
	}
	_, h, err := r.FormFile("file")
	if err != nil {
		return nil, fmt.Errorf("缺少 file / files 字段: %w", err)
	}
	if err := checkUploadLimits(in, []*multipart.FileHeader{h}); err != nil {
		return nil, err
	}
	p, err := s.saveFileHeader(h)
	if err != nil {
		return nil, err
	}
	return []string{p}, nil
}

// checkUploadLimits 按子功能 input 声明校验文件数与体积（声明了 multiple 才能多文件）。
func checkUploadLimits(in config.Input, headers []*multipart.FileHeader) error {
	if len(headers) > 1 && !in.Multiple {
		return fmt.Errorf("该子功能不支持多文件上传（收到 %d 个文件）", len(headers))
	}
	if max := in.FileCountLimit(); len(headers) > max {
		return fmt.Errorf("文件数 %d 超过上限 %d", len(headers), max)
	}
	var total int64
	for i, h := range headers {
		if limit := in.FileBytesLimit(); h.Size > limit {
			return fmt.Errorf("第 %d 个文件 %.1fMB 超过单文件上限 %.0fMB",
				i+1, float64(h.Size)/(1<<20), float64(limit)/(1<<20))
		}
		total += h.Size
	}
	if limit := in.TotalBytesLimit(); total > limit {
		return fmt.Errorf("上传总体积 %.1fMB 超过上限 %.0fMB",
			float64(total)/(1<<20), float64(limit)/(1<<20))
	}
	return nil
}

// saveFileHeader 将单个上传文件落盘，返回路径。
func (s *Server) saveFileHeader(header *multipart.FileHeader) (string, error) {
	file, err := header.Open()
	if err != nil {
		return "", err
	}
	defer file.Close()
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = ".png"
	}
	dst := filepath.Join(s.tmpDir, fmt.Sprintf("%d-%d%s", time.Now().UnixNano(), randInt(), ext))
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := out.ReadFrom(file); err != nil {
		return "", err
	}
	return dst, nil
}

// randInt 生成文件名后缀，避免同一纳秒内多文件重名。
func randInt() int64 {
	return time.Now().UnixNano() % 100000
}

// runPipelineWith 按给定依赖组装图并同步执行（deps 可携带进度回调，供异步任务使用）。
func (s *Server) runPipelineWith(deps nodes.Deps, featureID string, paths []string) (*nodes.State, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("未收到输入文件")
	}
	app, err := graph.Build(deps)
	if err != nil {
		return nil, fmt.Errorf("build graph: %w", err)
	}
	state := nodes.State{
		ImagePath:  paths[0], // 单页场景使用
		Pages:      paths,    // 多页场景使用
		RetryCount: map[string]int{},
	}
	final, err := app.Invoke(ctxBackground(), &state)
	if err != nil {
		return nil, err
	}
	fs, ok := final.(*nodes.State)
	if !ok {
		return nil, fmt.Errorf("unexpected final state type %T", final)
	}
	return fs, nil
}

// runPipeline 组装图并同步执行（按子功能取步骤配置、选择工作流）。
func (s *Server) runPipeline(featureID string, paths []string) (*nodes.State, error) {
	deps, ok := s.depsFor(featureID)
	if !ok {
		return nil, fmt.Errorf("子功能 %q 未启用或不存在", featureID)
	}
	return s.runPipelineWith(deps, featureID, paths)
}

// featureInfos 汇总子功能对外描述。
func (s *Server) featureInfos() []FeatureInfo {
	out := make([]FeatureInfo, 0, len(s.features))
	for _, f := range s.features {
		steps := make([]string, 0, len(f.Steps))
		for name := range f.Steps {
			steps = append(steps, name)
		}
		out = append(out, FeatureInfo{
			ID:          f.ID,
			Name:        f.Name,
			Description: f.Description,
			Icon:        f.Icon,
			Order:       f.Order,
			Kind:        orDefault(f.Kind, "workflow"),
			Pipeline:    f.Workflow.Pipeline,
			Steps:       steps,
			Input: FeatureInput{
				Kind:       orDefault(f.Input.Kind, "image"),
				Multiple:   f.Input.Multiple,
				Accept:     orDefault(f.Input.Accept, "image/png,image/jpeg,image/webp"),
				Hint:       f.Input.Hint,
				MaxFiles:   f.Input.FileCountLimit(),
				MaxFileMB:  int(f.Input.FileBytesLimit() >> 20),
				MaxTotalMB: int(f.Input.TotalBytesLimit() >> 20),
			},
			Output: FeatureOutput{
				Template:    orDefault(f.Output.Template, "markdown"),
				TemplateDir: f.Output.TemplateDir,
				Extension:   orDefault(f.Output.Extension, ".md"),
			},
			Async: s.cfg.Storage.PersistJobs && !f.IsChat(),
			Chat: FeatureChat{
				DefaultProvider: orDefault(f.Chat.DefaultProvider, s.cfg.LLM.DefaultProvider),
				StreamDefault:   s.chatStreamDefault(f),
				MaxHistory:      f.Chat.MaxHistory,
				MaxImages:       chatMaxImages(f),
				MaxImageMB:      chatMaxImageMB(f),
			},
		})
	}
	return out
}

// providerInfos 汇总 AI 端点对外描述（标记默认端点，隐藏 api-key）。
func (s *Server) providerInfos() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(s.cfg.LLM.Providers))
	for _, p := range s.cfg.LLM.Providers {
		out = append(out, ProviderInfo{
			Name:    p.Name,
			Type:    p.Type,
			BaseURL: p.BaseURL,
			Default: p.Name == s.cfg.LLM.DefaultProvider,
		})
	}
	return out
}

// toJob 将流水线结果转换为 API 响应体。
func toJob(id, featureID string, fs *nodes.State, err error, start time.Time) *JobResult {
	job := &JobResult{
		JobID:      id,
		FeatureID:  featureID,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
		return job
	}
	job.Status = "done"
	job.FinalReport = fs.FinalReport
	job.Structured = fs.Structured
	job.OCRText = fs.OCRText
	job.Translation = fs.Translation
	job.ReviewNotes = fs.ReviewNotes
	return job
}

// orDefault 空串回退到默认值。
func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// envOr 兜底环境变量读取。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

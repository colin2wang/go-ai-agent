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
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go-ai-agent/internal/config"
	"go-ai-agent/internal/graph"
	"go-ai-agent/internal/graph/nodes"
	"go-ai-agent/internal/iolog"
	"go-ai-agent/internal/llm"
	"go-ai-agent/internal/stats"
	"go-ai-agent/internal/tmpl"
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

	// Version 由 main 注入（ldflags 编译期版本）。
	Version string

	mu   sync.Mutex
	jobs map[string]*JobResult // jobID → 结果（内存态；重启即失）
}

// JobResult 一次子功能任务的执行结果。
type JobResult struct {
	JobID       string         `json:"job_id"`
	FeatureID   string         `json:"feature_id,omitempty"`
	Status      string         `json:"status"` // processing | done | failed
	FinalReport string         `json:"final_report,omitempty"`
	Structured  map[string]any `json:"structured,omitempty"`
	OCRText     string         `json:"ocr_text,omitempty"`
	Translation string         `json:"translation,omitempty"`
	ReviewNotes []string       `json:"review_notes,omitempty"`
	Error       string         `json:"error,omitempty"`
	DurationMS  int64          `json:"duration_ms"`
}

// FeatureInfo 子功能对外描述（前端菜单 / 上传控件 / 流程链）。
type FeatureInfo struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Icon        string        `json:"icon"`
	Order       int           `json:"order"`
	Pipeline    []string      `json:"pipeline"`
	Steps       []string      `json:"steps"`
	Input       FeatureInput  `json:"input"`
	Output      FeatureOutput `json:"output"`
}

// FeatureInput 输入声明（前端据此渲染上传控件）。
type FeatureInput struct {
	Kind     string `json:"kind"`
	Multiple bool   `json:"multiple"`
	Accept   string `json:"accept"`
	Hint     string `json:"hint"`
}

// FeatureOutput 输出声明（模板名 / 外部模板目录 / 下载扩展名）。
type FeatureOutput struct {
	Template    string `json:"template"`
	TemplateDir string `json:"template_dir"`
	Extension   string `json:"extension"`
}

// ProviderInfo AI 端点对外描述（不含 api-key）。
type ProviderInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	Default bool   `json:"default"`
}

// New 初始化 Server（工厂/统计/临时目录），为每个子功能预构建工作流依赖。
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
	tmpDir, err := os.MkdirTemp("", "harness-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create tmp dir: %w", err)
	}

	// 每个子功能一套步骤配置；未声明字段回退 ai.yml 全局默认
	pipelines := make(map[string]nodes.Deps, len(cfg.Resolved))
	for _, f := range cfg.Resolved {
		pipelines[f.ID] = nodes.Deps{
			Factory: factory,
			Steps:   f.Steps,
			Default: config.Step{
				Provider:    cfg.LLM.DefaultProvider,
				Model:       envOr("HARNESS_DEFAULT_MODEL", "qwen3-vl-8b"),
				Temperature: &cfg.LLM.Temperature,
				MaxTokens:   cfg.LLM.MaxTokens,
			},
			Stream:     cfg.LLM.StreamOutput,
			MaxRetries: f.Workflow.MaxRetries,
			Workflow:   f.Workflow.Type,
			// 模板引擎：内置模板 + 可选的外部目录覆盖
			Template: tmpl.New(f.Output.TemplateDir),
			Output:   f.Output,
		}
	}

	return &Server{
		cfg:       cfg,
		factory:   factory,
		recorder:  recorder,
		sink:      sink,
		tmpDir:    tmpDir,
		features:  cfg.Resolved,
		pipelines: pipelines,
		jobs:      map[string]*JobResult{},
	}, nil
}

// Close 释放资源。
func (s *Server) Close() {
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
func (s *Server) saveUploads(r *http.Request) ([]string, error) {
	if r.MultipartForm == nil {
		return nil, fmt.Errorf("缺少上传文件")
	}
	if headers := r.MultipartForm.File["files"]; len(headers) > 0 {
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
	p, err := s.saveFileHeader(h)
	if err != nil {
		return nil, err
	}
	return []string{p}, nil
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

// runPipeline 组装图并同步执行（按子功能取步骤配置、选择工作流）。
func (s *Server) runPipeline(featureID string, paths []string) (*nodes.State, error) {
	deps, ok := s.pipelines[featureID]
	if !ok {
		return nil, fmt.Errorf("子功能 %q 未启用或不存在", featureID)
	}
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
			Pipeline:    f.Workflow.Pipeline,
			Steps:       steps,
			Input: FeatureInput{
				Kind:     orDefault(f.Input.Kind, "image"),
				Multiple: f.Input.Multiple,
				Accept:   orDefault(f.Input.Accept, "image/png,image/jpeg,image/webp"),
				Hint:     f.Input.Hint,
			},
			Output: FeatureOutput{
				Template:    orDefault(f.Output.Template, "markdown"),
				TemplateDir: f.Output.TemplateDir,
				Extension:   orDefault(f.Output.Extension, ".md"),
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

// Package api 提供 REST API：上传工资单图片 → 执行审读流水线 → 返回报告。
// Swagger 注解经 swaggo 生成 OpenAPI 文档（/swagger/index.html）。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go-ai-agent/internal/config"
	"go-ai-agent/internal/graph"
	"go-ai-agent/internal/graph/nodes"
	"go-ai-agent/internal/iolog"
	"go-ai-agent/internal/llm"
	"go-ai-agent/internal/stats"
)

// Server HTTP 服务，内部持有已初始化的流水线依赖。
type Server struct {
	cfg     *config.Config
	factory *llm.Factory
	deps    nodes.Deps
	sink    *llm.StatsSink
	tmpDir  string

	mu   sync.Mutex
	jobs map[string]*JobResult // jobID → 结果（内存态；重启即失）
}

// JobResult 一次审读任务的执行结果。
type JobResult struct {
	JobID       string         `json:"job_id"`
	Status      string         `json:"status"` // processing | done | failed
	FinalReport string         `json:"final_report,omitempty"`
	Structured  map[string]any `json:"structured,omitempty"`
	OCRText     string         `json:"ocr_text,omitempty"`
	Translation string         `json:"translation,omitempty"`
	ReviewNotes []string       `json:"review_notes,omitempty"`
	Error       string         `json:"error,omitempty"`
	DurationMS  int64          `json:"duration_ms"`
}

// New 初始化 Server（工厂/统计/临时目录），与 cmd/harness 的装配逻辑一致。
func New(cfg *config.Config) (*Server, error) {
	var recorder *stats.Recorder
	var sink *llm.StatsSink
	if cfg.LLMStats.Enabled {
		var err error
		recorder, err = stats.Open(cfg.LLMStats.DBPath, cfg.LLMStats.RetentionDays)
		if err != nil {
			return nil, fmt.Errorf("init llm-stats: %w", err)
		}
		sink = llm.NewStatsSink(recorder, cfg.LLMStats.QueueCapacity)
	}
	factory, err := llm.NewFactory(cfg.LLM.Providers, sink, iolog.New(cfg.LLM.IoLog).Hook())
	if err != nil {
		return nil, fmt.Errorf("init llm factory: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "harness-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create tmp dir: %w", err)
	}
	deps := nodes.Deps{
		Factory: factory,
		Steps:   cfg.LLM.Steps,
		Default: config.Step{
			Provider:    cfg.LLM.DefaultProvider,
			Model:       envOr("HARNESS_DEFAULT_MODEL", "qwen3-vl-8b"),
			Temperature: cfg.LLM.Temperature,
			MaxTokens:   cfg.LLM.MaxTokens,
		},
		Stream: cfg.LLM.StreamOutput,
	}
	if sink != nil {
		// Server 生命周期结束统一释放（http 服务常驻时可不 Close）
		_ = recorder
	}
	return &Server{cfg: cfg, factory: factory, deps: deps, sink: sink, tmpDir: tmpDir, jobs: map[string]*JobResult{}}, nil
}

// Close 释放资源。
func (s *Server) Close() {
	if s.sink != nil {
		s.sink.Close()
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

// saveUpload 保存上传文件到临时目录，返回落盘路径。
func (s *Server) saveUpload(r *http.Request) (string, error) {
	file, header, err := r.FormFile("file")
	if err != nil {
		return "", fmt.Errorf("缺少 file 字段: %w", err)
	}
	defer file.Close()
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = ".png"
	}
	dst := filepath.Join(s.tmpDir, fmt.Sprintf("%d%s", time.Now().UnixNano(), ext))
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

// runPipeline 组装图并同步执行（异步任务内部也复用此函数）。
func (s *Server) runPipeline(imagePath string) (*nodes.State, error) {
	app, err := graph.Build(s.deps)
	if err != nil {
		return nil, fmt.Errorf("build graph: %w", err)
	}
	state := nodes.State{ImagePath: imagePath, RetryCount: map[string]int{}}
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

// toJob 将流水线结果转换为 API 响应体。
func toJob(id string, fs *nodes.State, err error, start time.Time) *JobResult {
	job := &JobResult{
		JobID:      id,
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

// envOr 兜底环境变量读取。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

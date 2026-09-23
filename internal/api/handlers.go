package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	httpSwagger "github.com/swaggo/http-swagger"

	_ "go-ai-agent/docs" // swag 生成的 OpenAPI 文档
	"go-ai-agent/internal/llm"
	"go-ai-agent/internal/stats"
)

// ctxBackground 独立函数便于 mock/超时替换。
func ctxBackground() context.Context { return context.Background() }

// Register 挂载全部路由。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /swagger/", httpSwagger.WrapHandler)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/features", s.handleFeatures)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("POST /api/providers/check", s.handleProviderCheck)
	mux.HandleFunc("POST /api/features/{id}/run", s.handleRunFeature)
	mux.HandleFunc("POST /api/payslip/review", s.handlePayslipReview) // 工资单场景快捷入口
	mux.HandleFunc("GET /api/jobs/{id}", s.handleJob)
	// 内嵌 SPA 前端（根路径及未知路径回退 index.html）
	mux.Handle("GET /", staticHandler())
}

// ServeHTTP 实现 http.Handler（供测试与嵌入使用）。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	s.Register(mux)
	mux.ServeHTTP(w, r)
}

// handleHealth godoc
// @Summary      健康检查
// @Description  服务存活探测
// @Tags         system
// @Produce      json
// @Success      200  {object}  map[string]string
// @Router       /api/health [get]
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]string{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

// handleFeatures godoc
// @Summary      子功能清单
// @Description  返回已启用的子功能（前端左侧菜单与流程链展示）
// @Tags         features
// @Produce      json
// @Success      200  {object}  map[string][]FeatureInfo
// @Router       /api/features [get]
func (s *Server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]any{
		"app":       map[string]string{"name": s.cfg.App.Name, "title": s.cfg.App.Title, "version": s.Version},
		"features":  s.featureInfos(),
		"providers": s.providerInfos(),
	})
}

// handleOverview godoc
// @Summary      概览指标
// @Description  最近 N 天（query: days，默认 14）调用统计聚合：总量/今日/耗时/成功率/趋势/模型用量/最近调用
// @Tags         overview
// @Produce      json
// @Param        days  query  int  false  "统计天数（默认 14）"
// @Success      200  {object}  map[string]any
// @Router       /api/overview [get]
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 180 {
			days = n
		}
	}
	var ov *stats.Overview
	if s.recorder != nil {
		var err error
		ov, err = s.recorder.Overview(days)
		if err != nil {
			fail(w, http.StatusInternalServerError, "聚合统计失败: "+err.Error())
			return
		}
	}
	respond(w, http.StatusOK, map[string]any{
		"app":         map[string]string{"name": s.cfg.App.Name, "title": s.cfg.App.Title, "version": s.Version},
		"providers":   s.providerInfos(),
		"stats":       ov,
		"days":        days,
		"server_time": time.Now().Format(time.RFC3339),
	})
}

// handleProviderCheck godoc
// @Summary      测试 AI 端点连通性
// @Description  对 ai.yml 声明的 provider 做一次连通性探测（GET {base-url}/v1/models）。
// @Description  body: {"provider":"name"} 仅测指定端点，省略或为空则并发测试全部端点。
// @Description  由前端在概览页加载时调用一次、或用户手动点击「测试 / 全部测试」时调用，不做后台实时轮询。
// @Tags         providers
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]any  "results: llm.ProbeResult 数组（provider/status/http_status/latency_ms/detail/checked_at）"
// @Router       /api/providers/check [post]
func (s *Server) handleProviderCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	// 允许空 body：视为测试全部端点
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	names := make([]string, 0, len(s.cfg.LLM.Providers))
	if req.Provider != "" {
		names = append(names, req.Provider)
	} else {
		for _, p := range s.cfg.LLM.Providers {
			names = append(names, p.Name)
		}
	}

	// 并发探测，保持与请求顺序一致
	results := make([]llm.ProbeResult, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i] = s.factory.Probe(name)
		}(i, name)
	}
	wg.Wait()

	respond(w, http.StatusOK, map[string]any{"results": results})
}

// handleRunFeature godoc
// @Summary      执行子功能工作流
// @Description  multipart 上传文件并按子功能 id 执行其工作流。单文件用 file 字段；多页场景用 files 字段（可多个，按上传顺序处理）
// @Tags         features
// @Accept       multipart/form-data
// @Produce      json
// @Param        id     path      string  true   "子功能 ID（如 payslip-ocr / book-toc）"
// @Param        file   formData  file    false  "单文件输入（如工资单图片）"
// @Param        files  formData  file    false  "多页输入（如多张目录页扫描件，可多个）"
// @Success      200   {object}  JobResult
// @Failure      400   {object}  apiError
// @Failure      404   {object}  apiError
// @Router       /api/features/{id}/run [post]
func (s *Server) handleRunFeature(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.pipelines[id]; !ok {
		fail(w, http.StatusNotFound, "子功能未启用或不存在: "+id)
		return
	}
	s.runFeature(w, r, id)
}

// handlePayslipReview godoc
// @Summary      上传工资单并执行审读流水线
// @Description  multipart/form-data 上传工资单图片（字段名 file），执行 payslip-ocr 子功能工作流
// @Tags         payslip
// @Accept       multipart/form-data
// @Produce      json
// @Param        file  formData  file  true  "工资单图片（PNG/JPG）"
// @Success      200   {object}  JobResult
// @Failure      400   {object}  apiError
// @Failure      500   {object}  apiError
// @Router       /api/payslip/review [post]
func (s *Server) handlePayslipReview(w http.ResponseWriter, r *http.Request) {
	const id = "payslip-ocr"
	if _, ok := s.pipelines[id]; !ok {
		fail(w, http.StatusNotFound, "子功能未启用或不存在: "+id)
		return
	}
	s.runFeature(w, r, id)
}

// runFeature 上传 + 执行 + 记录任务结果的公共流程。
func (s *Server) runFeature(w http.ResponseWriter, r *http.Request, featureID string) {
	if err := r.ParseMultipartForm(32 << 20); err != nil { // 32MB
		fail(w, http.StatusBadRequest, "解析 multipart 失败: "+err.Error())
		return
	}
	paths, err := s.saveUploads(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	start := time.Now()
	fs, runErr := s.runPipeline(featureID, paths)
	job := toJob(newJobID(), featureID, fs, runErr, start)
	s.mu.Lock()
	s.jobs[job.JobID] = job
	s.mu.Unlock()
	respond(w, http.StatusOK, job)
}

// handleJob godoc
// @Summary      查询任务结果
// @Description  按 jobID 查询一次执行的结果（当前实现为同步接口，此端点预留异步扩展）
// @Tags         jobs
// @Produce      json
// @Param        id  path  string  true  "任务 ID"
// @Success      200 {object}  JobResult
// @Failure      404 {object}  apiError
// @Router       /api/jobs/{id} [get]
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	job, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, "job not found: "+id)
		return
	}
	respond(w, http.StatusOK, job)
}

// newJobID 生成任务 ID（时间戳 + 计数，简单可读）。
func newJobID() string {
	return fmt.Sprintf("job-%d", time.Now().UnixNano())
}

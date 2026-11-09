package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	httpSwagger "github.com/swaggo/http-swagger"

	_ "go-ai-agent/api" // swag 生成的 OpenAPI 文档
	"go-ai-agent/internal/config"
	"go-ai-agent/internal/llm"
	"go-ai-agent/internal/stats"
)

// ctxBackground 独立函数便于 mock/超时替换。
func ctxBackground() context.Context { return context.Background() }

// Register 挂载全部路由。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /swagger/", s.handleSwagger)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/features", s.handleFeatures)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("POST /api/providers/check", s.handleProviderCheck)
	mux.HandleFunc("GET /api/providers/models", s.handleChatModels)            // 对话：模型列表
	mux.HandleFunc("GET /api/providers/caps", s.handleModelCaps)               // 对话：模型能力探测（带缓存）
	mux.HandleFunc("GET /api/providers/stream-check", s.handleChatStreamCheck) // 对话：流式能力探测
	mux.HandleFunc("POST /api/chat/send", s.handleChatSend)                    // 对话：SSE 流式回复
	mux.HandleFunc("POST /api/features/{id}/run", s.handleRunFeature)
	mux.HandleFunc("POST /api/payslip/review", s.handlePayslipReview) // 工资单场景快捷入口
	mux.HandleFunc("GET /api/jobs", s.handleJobList)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleJob)
	mux.HandleFunc("GET /api/jobs/{id}/events", s.handleJobEvents) // 异步任务进度 SSE
	// 内嵌 SPA 前端（根路径及未知路径回退 index.html）
	mux.Handle("GET /", staticHandler())
}

// handleSwagger Swagger UI：配置关闭时返回 404，避免在生产环境暴露接口文档。
func (s *Server) handleSwagger(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Server.SwaggerOn() {
		fail(w, http.StatusNotFound, "接口文档已在配置中关闭（server.swagger-enabled: false）")
		return
	}
	// httpSwagger.WrapHandler 本身就是 http.HandlerFunc（签名 func(w, r)），
	// 直接以 (w, r) 调用即可，不需要也不能先调用一次拿返回值。
	httpSwagger.WrapHandler(w, r)
}

// ServeHTTP 实现 http.Handler；配置了 server.api-tokens 时对 /api/* 强制鉴权
// （健康检查可用 auth-exclude-health 豁免；前端页面与静态资源不鉴权）。
// 路由树与鉴权包装只构建一次，后续请求直接复用。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.routeOnce.Do(func() {
		mux := http.NewServeMux()
		s.Register(mux)
		if len(s.cfg.Server.APITokens) == 0 {
			s.router = mux
			return
		}
		s.router = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.authRequired(r) || s.tokenValid(r) {
				mux.ServeHTTP(w, r)
				return
			}
			fail(w, http.StatusUnauthorized, "缺少或无效的 API 令牌（Authorization: Bearer <token>）")
		})
	})
	s.router.ServeHTTP(w, r)
}

// authRequired 判定请求是否需要鉴权：仅 /api/* 参与，可选豁免 /api/health。
func (s *Server) authRequired(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	if s.cfg.Server.AuthExcludeHealth && r.URL.Path == "/api/health" {
		return false
	}
	return true
}

// tokenValid 校验 Authorization: Bearer 与 X-API-Key 两种头，常数时间比对。
func (s *Server) tokenValid(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.Header.Get("X-API-Key")
	}
	if got == "" {
		return false
	}
	for _, t := range s.cfg.Server.APITokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(got)) == 1 {
			return true
		}
	}
	return false
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
// @Description  multipart 上传文件并按子功能 id 执行其工作流。单文件用 file 字段；多页场景用 files 字段（可多个，按上传顺序处理）。
// @Description  启用 storage.persist-jobs 后默认异步：立即返回 job_id（status=processing），前端轮询 GET /api/jobs/{id} 获取进度与结果；
// @Description  可用 query 参数 async=1 / async=0 强制单次请求的同步/异步行为。
// @Tags         features
// @Accept       multipart/form-data
// @Produce      json
// @Param        id     path      string  true   "子功能 ID（如 payslip-ocr / book-toc）"
// @Param        async  query     string  false  "async=1 强制异步 / async=0 强制同步"
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
	if f, ok := s.cfg.Feature(id); ok && f.IsChat() {
		fail(w, http.StatusBadRequest, "对话类子功能不支持上传执行，请使用 /api/chat/send")
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

// collectFormParams 收集 multipart 表单中的非文件字段（如 target_lang）。
// 限制字段数与值长度，避免超长文本被拼进 LLM 提示词。
func collectFormParams(r *http.Request) map[string]string {
	out := map[string]string{}
	if r.MultipartForm == nil {
		return out
	}
	const (
		maxParams   = 16
		maxValueLen = 128
	)
	for k, vals := range r.MultipartForm.Value {
		if len(out) >= maxParams {
			break
		}
		if len(vals) == 0 || vals[0] == "" {
			continue
		}
		v := []rune(strings.TrimSpace(vals[0]))
		if len(v) > maxValueLen {
			v = v[:maxValueLen]
		}
		out[k] = string(v)
	}
	return out
}

// validateFormParams 按子功能声明校验额外表单参数：目标语言未提交时用
// yml 的 default 补齐，且必须是 options 白名单内的值（启动期已校验 default 合法）。
func validateFormParams(params map[string]string, in config.Input) error {
	sel := in.TargetLanguage
	if sel == nil {
		return nil
	}
	lang := params[config.TargetLangField]
	if lang == "" {
		lang = sel.Default
		params[config.TargetLangField] = lang
	}
	for _, o := range sel.Options {
		if o == lang {
			return nil
		}
	}
	return fmt.Errorf("目标语言 %q 不在允许的选项中", lang)
}

// runFeature 上传 + 执行 + 记录任务结果的公共流程。
// 异步模式（persist-jobs 开启或请求 async=1）：登记任务后立即返回 job_id，
// 后台执行并把进度写入 jobs 表；同步模式保持旧行为。
func (s *Server) runFeature(w http.ResponseWriter, r *http.Request, featureID string) {
	in := config.Input{}
	if f, ok := s.cfg.Feature(featureID); ok {
		in = f.Input
	}
	// 请求体硬上限：比声明的总体积上限多留 8MB 余量给 multipart 边界开销
	r.Body = http.MaxBytesReader(w, r.Body, in.TotalBytesLimit()+(8<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil { // 内存缓冲 32MB，超出落磁盘临时文件
		fail(w, http.StatusBadRequest, "解析 multipart 失败: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	paths, err := s.saveUploads(r, in)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 额外表单参数（目标语言等）：先补默认值再按 yml 白名单校验，
	// 避免任意文本进入 LLM 提示词。
	params := collectFormParams(r)
	if err := validateFormParams(params, in); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	if s.asyncRun(featureID, r) {
		job, err := s.startAsyncJob(featureID, paths, len(paths), params)
		if err != nil {
			fail(w, http.StatusInternalServerError, "登记异步任务失败: "+err.Error())
			return
		}
		respond(w, http.StatusAccepted, job)
		return
	}

	start := time.Now()
	fs, runErr := s.runPipeline(featureID, paths, params)
	job := toJob(newJobID(), featureID, fs, runErr, start)
	s.mu.Lock()
	s.rememberJobLocked(job)
	s.mu.Unlock()
	respond(w, http.StatusOK, job)
}

// handleJob godoc
// @Summary      查询任务结果
// @Description  按 jobID 查询一次执行的结果与进度（异步任务轮询此接口；启用 persist-jobs 后重启仍可查询历史任务）
// @Tags         jobs
// @Produce      json
// @Param        id  path  string  true  "任务 ID"
// @Success      200 {object}  JobResult
// @Failure      404 {object}  apiError
// @Router       /api/jobs/{id} [get]
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.lookupJob(id)
	if !ok {
		fail(w, http.StatusNotFound, "job not found: "+id)
		return
	}
	respond(w, http.StatusOK, job)
}

// jobSeq 保证同一纳秒内并发生成的任务 ID 仍唯一。
var jobSeq atomic.Uint64

// newJobID 生成任务 ID（时间戳 + 全局计数，简单可读且无碰撞）。
func newJobID() string {
	return fmt.Sprintf("job-%d-%d", time.Now().UnixNano(), jobSeq.Add(1))
}

package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	httpSwagger "github.com/swaggo/http-swagger"

	_ "go-ai-agent/docs" // swag 生成的 OpenAPI 文档
)

// ctxBackground 独立函数便于 mock/超时替换。
func ctxBackground() context.Context { return context.Background() }

// Register 挂载全部路由。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /swagger/", httpSwagger.WrapHandler)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/payslip/review", s.handleReview)
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

// handleReview godoc
// @Summary      上传工资单并执行审读流水线
// @Description  multipart/form-data 上传工资单图片（字段名 file），同步执行 OCR→翻译→审读→专家→汇总，返回完整报告
// @Tags         payslip
// @Accept       multipart/form-data
// @Produce      json
// @Param        file  formData  file  true  "工资单图片（PNG/JPG）"
// @Success      200   {object}  JobResult
// @Failure      400   {object}  apiError
// @Failure      500   {object}  apiError
// @Router       /api/payslip/review [post]
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil { // 32MB
		fail(w, http.StatusBadRequest, "解析 multipart 失败: "+err.Error())
		return
	}
	path, err := s.saveUpload(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	start := time.Now()
	fs, err := s.runPipeline(path)
	respond(w, http.StatusOK, toJob(newJobID(), fs, err, start))
}

// handleJob godoc
// @Summary      查询任务结果
// @Description  按 jobID 查询一次审读的结果（当前实现为同步接口，此端点预留异步扩展）
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

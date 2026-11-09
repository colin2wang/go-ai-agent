// jobs.go 异步任务 API：POST run?async=1 立即返回 job_id，后台执行并持续把
// 进度写入 SQLite jobs 表；前端通过 GET /api/jobs/{id} 轮询或 GET /api/jobs/{id}/events
// 订阅 SSE 获取“第 n/m 页”进度与最终结果。未启用 persist-jobs 时全部回退旧的
// 同步 + 内存态行为，保证单机零配置体验不变。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go-ai-agent/internal/graph/nodes"
	"go-ai-agent/internal/stats"
)

// jobResultPayload 存进 jobs.result 的载荷（完成时的最终结果字段）。
type jobResultPayload struct {
	FinalReport string         `json:"final_report,omitempty"`
	Structured  map[string]any `json:"structured,omitempty"`
	OCRText     string         `json:"ocr_text,omitempty"`
	Translation string         `json:"translation,omitempty"`
	ReviewNotes []string       `json:"review_notes,omitempty"`
}

// encodeJobResult 把流水线终态编码为 result BLOB（编码失败视为任务失败信息）。
func encodeJobResult(fs *nodes.State) []byte {
	if fs == nil {
		return nil
	}
	b, err := json.Marshal(jobResultPayload{
		FinalReport: fs.FinalReport,
		Structured:  fs.Structured,
		OCRText:     fs.OCRText,
		Translation: fs.Translation,
		ReviewNotes: fs.ReviewNotes,
	})
	if err != nil {
		return nil
	}
	return b
}

// startAsyncJob 登记任务并后台执行，返回初始任务快照（status=processing）。
// 存储写入失败时降级为内存登记（GET 仍可查到本轮结果）。
func (s *Server) startAsyncJob(featureID string, paths []string, total int, params map[string]string) (*JobResult, error) {
	store := s.jobsStore()
	id := newJobID()
	now := time.Now().Format(time.RFC3339)
	job := &JobResult{
		JobID: id, FeatureID: featureID, Status: "processing",
		Total: total, Detail: "任务已受理，等待执行",
		CreatedAt: now, UpdatedAt: now,
	}
	if store != nil {
		if err := store.Create(id, featureID, total); err != nil {
			return nil, err
		}
	} else {
		// 未落库时先同步登记内存，避免响应返回后立刻轮询拿到 404
		s.mu.Lock()
		s.rememberJobLocked(job)
		s.mu.Unlock()
	}
	s.numWorking.Add(1)
	go s.runAsyncJob(id, featureID, paths, total, params)
	return job, nil
}

// runAsyncJob 后台执行流水线：进度写 jobs 表，终态写结果/错误编码。
func (s *Server) runAsyncJob(id, featureID string, paths []string, total int, params map[string]string) {
	defer s.numWorking.Add(-1)
	store := s.jobsStore()
	start := time.Now()

	// mark 更新任务行（store 为 nil 时仅内存态，同步路径不会走到这里）
	mark := func(status string, progress int, detail string, result []byte, errMsg string) {
		if store == nil {
			return
		}
		if err := store.Update(id, status, progress, total, detail, result, errMsg); err != nil {
			slog.Warn("更新任务状态失败", slog.String("job", id), slog.String("error", err.Error()))
		}
	}
	// 未开持久化时靠内存 map 供 GET 查询（开持久化时不写内存，避免大结果长期驻留）
	keepMem := store == nil
	snapshot := func(status string, progress int, detail string) {
		if !keepMem {
			return
		}
		s.mu.Lock()
		if j, ok := s.jobs[id]; ok {
			j.Status, j.Progress, j.Detail = status, progress, detail
		} else {
			s.rememberJobLocked(&JobResult{JobID: id, FeatureID: featureID, Status: status,
				Progress: progress, Total: total, Detail: detail})
		}
		s.mu.Unlock()
	}

	deps := s.buildDepsForProgress(featureID, func(step string, done, tot int, detail string) {
		if done < 0 {
			done = 0
		}
		if tot > 0 {
			total = tot
		}
		mark("running", done, detail, nil, "")
		snapshot("running", done, detail)
	}, params)
	mark("running", 0, "开始执行", nil, "")
	snapshot("running", 0, "开始执行")

	fs, runErr := s.runPipelineWith(deps, featureID, paths)
	job := toJob(id, featureID, fs, runErr, start)
	if runErr != nil {
		mark("failed", 0, "", nil, runErr.Error())
	} else {
		mark("done", total, "完成", encodeJobResult(fs), "")
	}
	if keepMem {
		s.mu.Lock()
		s.rememberJobLocked(job)
		s.mu.Unlock()
	}
	slog.Info("异步任务结束", slog.String("job", id), slog.String("feature", featureID),
		slog.String("status", job.Status), slog.Int64("duration_ms", job.DurationMS))
}

// maxMemJobs 未启用任务落库时内存保留的任务数上限（防止长时间运行无界增长）。
const maxMemJobs = 100

// rememberJobLocked 写入内存任务表并按插入顺序淘汰旧项（调用方须已持 s.mu）。
func (s *Server) rememberJobLocked(job *JobResult) {
	if _, ok := s.jobs[job.JobID]; !ok {
		s.jobOrder = append(s.jobOrder, job.JobID)
	}
	s.jobs[job.JobID] = job
	if over := len(s.jobOrder) - maxMemJobs; over > 0 {
		for _, id := range s.jobOrder[:over] {
			delete(s.jobs, id)
		}
		s.jobOrder = append(s.jobOrder[:0], s.jobOrder[over:]...)
	}
}

// buildDepsForProgress 为一次异步执行构造带进度回调的依赖（total 动态回填写入行）。
func (s *Server) buildDepsForProgress(featureID string, progress func(step string, done, total int, detail string), params map[string]string) nodes.Deps {
	f, ok := s.cfg.Feature(featureID)
	if !ok {
		if d, has := s.pipelines[featureID]; has {
			d.Progress = progress
			d.Params = params
			return d
		}
	}
	return s.buildDeps(f, progress, params)
}

// jobView 从存储行还原任务快照（result 仅在 done 时解码）。
func jobView(row stats.JobRow) *JobResult {
	out := &JobResult{
		JobID:     row.ID,
		FeatureID: row.FeatureID,
		Status:    row.Status,
		Progress:  row.Progress,
		Total:     row.Total,
		Detail:    row.Detail,
		Error:     row.Error,
		CreatedAt: row.CreatedAt.Format(time.RFC3339),
		UpdatedAt: row.UpdatedAt.Format(time.RFC3339),
	}
	if !row.CreatedAt.IsZero() && !row.UpdatedAt.IsZero() {
		out.DurationMS = row.UpdatedAt.Sub(row.CreatedAt).Milliseconds()
	}
	if row.Status == "done" && len(row.Result) > 0 {
		var p jobResultPayload
		if json.Unmarshal(row.Result, &p) == nil {
			out.FinalReport = p.FinalReport
			out.Structured = p.Structured
			out.OCRText = p.OCRText
			out.Translation = p.Translation
			out.ReviewNotes = p.ReviewNotes
		}
	}
	return out
}

// lookupJob 查询任务：优先 SQLite（重启后仍可查），回退内存态。
func (s *Server) lookupJob(id string) (*JobResult, bool) {
	if store := s.jobsStore(); store != nil {
		row, err := store.Get(id)
		if err == nil {
			return jobView(row), true
		}
		if !errors.Is(err, stats.ErrJobNotFound) {
			slog.Warn("查询任务失败", slog.String("job", id), slog.String("error", err.Error()))
		}
		// 库中确无此任务时再查内存（异步任务写库失败的降级场景）
	}
	s.mu.Lock()
	job, ok := s.jobs[id]
	s.mu.Unlock()
	return job, ok
}

// handleJobList godoc
// @Summary      任务列表
// @Description  返回异步任务（不含结果正文），支持过滤与分页：
// @Description  query: feature（子功能 id）、status（pending/running/done/failed）、n（每页条数，默认 50，上限 200）、offset（偏移量）
// @Tags         jobs
// @Produce      json
// @Param        feature  query  string  false  "按子功能 id 过滤"
// @Param        status   query  string  false  "按状态过滤"
// @Param        n        query  int     false  "每页条数（默认 50，上限 200）"
// @Param        offset   query  int     false  "分页偏移量"
// @Success      200  {object}  map[string]any  "jobs: JobResult 数组；total: 过滤条件下的总条数"
// @Router       /api/jobs [get]
func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	store := s.jobsStore()
	if store == nil {
		respond(w, http.StatusOK, map[string]any{"jobs": []JobResult{}, "persistent": false})
		return
	}
	q := r.URL.Query()
	n := 50
	if v := q.Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			offset = parsed
		}
	}
	rows, err := store.Query(stats.JobFilter{
		Feature: q.Get("feature"),
		Status:  q.Get("status"),
		Limit:   n,
		Offset:  offset,
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]JobResult, 0, len(rows))
	for _, row := range rows {
		view := jobView(row)
		out = append(out, *view)
	}
	respond(w, http.StatusOK, map[string]any{
		"jobs":       out,
		"persistent": true,
		"limit":      n,
		"offset":     offset,
	})
}

// handleJobEvents godoc
// @Summary      任务进度流（SSE）
// @Description  以 SSE 推送任务进度与终态：event: progress / done / failed，data 为 JobResult；任务已结束时收到终态后立即关闭
// @Tags         jobs
// @Produce      text/event-stream
// @Param        id  path  string  true  "任务 ID"
// @Success      200  {string}  string  "SSE 事件流"
// @Failure      404  {object}  apiError
// @Router       /api/jobs/{id}/events [get]
func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "当前环境不支持 SSE")
		return
	}
	// 首次查询失败直接 404（后续查询错误只结束流）
	first, found := s.lookupJob(id)
	if !found {
		fail(w, http.StatusNotFound, "job not found: "+id)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func(event string, job *JobResult) {
		b, err := json.Marshal(job)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	prev := *first
	for {
		event := "progress"
		switch prev.Status {
		case "done":
			event = "done"
		case "failed":
			event = "failed"
		}
		send(event, &prev)
		if prev.Status == "done" || prev.Status == "failed" {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		job, found := s.lookupJob(id)
		if !found {
			return
		}
		prev = *job
	}
}

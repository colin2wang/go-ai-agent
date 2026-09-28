package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-ai-agent/internal/config"
	"go-ai-agent/internal/graph/nodes"
	"go-ai-agent/internal/stats"
)

// newStorageServer 构造带 SQLite 的 Server（persist 控制是否开启任务落库）。
func newStorageServer(t *testing.T, persist bool) *Server {
	t.Helper()
	cfg := &config.Config{
		LLM: config.LLM{
			Providers: []config.Provider{
				{Name: "local", Type: "openai", BaseURL: "http://localhost:1234/v1", APIKey: "k"},
			},
		},
		Storage: config.Storage{
			Enabled:     true,
			DBPath:      filepath.Join(t.TempDir(), "api.db"),
			PersistJobs: persist,
		},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// waitTerminal 轮询直到任务进入终态（避免测试依赖固定耗时）。
func waitTerminal(t *testing.T, s *Server, id string) *JobResult {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := s.lookupJob(id)
		if ok && (job.Status == "done" || job.Status == "failed") {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("任务 %s 未能在超时前进入终态", id)
	return nil
}

// ---- 结果编解码 ----

func TestJobResultPayloadRoundTrip(t *testing.T) {
	st := &nodes.State{
		FinalReport: "报告正文",
		OCRText:     "ocr 原文",
		Translation: "译文",
		ReviewNotes: []string{"note-1", "note-2"},
		Structured:  map[string]any{"amount": 123},
	}
	blob := encodeJobResult(st)
	if len(blob) == 0 {
		t.Fatal("编码结果不应为空")
	}
	view := jobView(stats.JobRow{ID: "j", Status: "done", Result: blob})
	if view.FinalReport != "报告正文" || view.OCRText != "ocr 原文" || view.Translation != "译文" {
		t.Errorf("往返不一致: %+v", view)
	}
	if len(view.ReviewNotes) != 2 || view.Structured["amount"] != float64(123) {
		t.Errorf("结构化字段不一致: %+v", view)
	}
}

func TestJobViewSkipsResultUnlessDone(t *testing.T) {
	blob := encodeJobResult(&nodes.State{FinalReport: "x"})
	// 非终态不应解码 result（避免把中间态结果提前暴露给前端）
	view := jobView(stats.JobRow{ID: "j", Status: "running", Result: blob})
	if view.FinalReport != "" {
		t.Errorf("running 状态不应带出结果，得到 %q", view.FinalReport)
	}
}

func TestEncodeJobResultNilState(t *testing.T) {
	if blob := encodeJobResult(nil); blob != nil {
		t.Error("nil state 应编码为 nil")
	}
}

// ---- 异步任务生命周期 ----

func TestAsyncJobReachesTerminalState(t *testing.T) {
	s := newStorageServer(t, true)
	job, err := s.startAsyncJob("not-registered", []string{"nonexistent.png"}, 2)
	if err != nil {
		t.Fatalf("startAsyncJob: %v", err)
	}
	if job.Status != "processing" {
		t.Errorf("初始状态 = %q，期望 processing", job.Status)
	}
	if job.JobID == "" {
		t.Fatal("应返回 job_id")
	}

	final := waitTerminal(t, s, job.JobID)
	// 未注册的子功能会失败，但要确保状态机推进到终态而不是卡住
	if final.Status != "failed" {
		t.Errorf("终态 = %q，期望 failed", final.Status)
	}
	if final.Error == "" {
		t.Error("失败任务应带错误信息")
	}
	// 落库后由 SQLite 读取，重启仍可查
	row, gerr := s.jobsStore().Get(job.JobID)
	if gerr != nil {
		t.Fatalf("任务应已落库: %v", gerr)
	}
	if row.Status != "failed" {
		t.Errorf("库中状态 = %q", row.Status)
	}
}

func TestAsyncRunQueryOverride(t *testing.T) {
	s := newStorageServer(t, false) // 全局未开持久化
	req := httptest.NewRequest("GET", "/api/features/x/run?async=1", nil)
	if !s.asyncRun("x", req) {
		t.Error("async=1 应强制异步")
	}
	req0 := httptest.NewRequest("GET", "/api/features/x/run?async=0", nil)
	if s.asyncRun("x", req0) {
		t.Error("async=0 应强制同步")
	}
	reqDef := httptest.NewRequest("GET", "/api/features/x/run", nil)
	if s.asyncRun("x", reqDef) {
		t.Error("未开启 persist-jobs 时默认应同步")
	}
}

func TestJobIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := newJobID()
		if seen[id] {
			t.Fatalf("任务 ID 重复: %s", id)
		}
		seen[id] = true
	}
}

// ---- 内存态回退（未开持久化）----

func TestMemoryJobEviction(t *testing.T) {
	s := newStorageServer(t, false)
	for i := 0; i < maxMemJobs+20; i++ {
		s.mu.Lock()
		s.rememberJobLocked(&JobResult{JobID: "job-" + strconv.Itoa(i)})
		s.mu.Unlock()
	}
	s.mu.Lock()
	n := len(s.jobs)
	order := len(s.jobOrder)
	s.mu.Unlock()
	if n > maxMemJobs {
		t.Errorf("内存任务数 = %d，应不超过 %d", n, maxMemJobs)
	}
	if n != order {
		t.Errorf("jobOrder(%d) 与 jobs(%d) 长度应一致", order, n)
	}
}

func TestLookupJobFallsBackToMemory(t *testing.T) {
	// 未开持久化：jobsStore() 为 nil，查询走内存
	s := newStorageServer(t, false)
	s.mu.Lock()
	s.rememberJobLocked(&JobResult{JobID: "mem-1", Status: "done"})
	s.mu.Unlock()
	job, ok := s.lookupJob("mem-1")
	if !ok || job.Status != "done" {
		t.Errorf("内存态任务应可查，得到 %v / %v", job, ok)
	}
	if _, ok := s.lookupJob("absent"); ok {
		t.Error("不存在的任务不应命中")
	}
}

// ---- HTTP 端点 ----

func TestJobEndpointNotFound(t *testing.T) {
	s := newStorageServer(t, true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/jobs/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("状态码 = %d，期望 404", resp.StatusCode)
	}
}

func TestJobListEndpoint(t *testing.T) {
	s := newStorageServer(t, true)
	store := s.jobsStore()
	if store == nil {
		t.Fatal("持久化开启时 jobsStore 不应为 nil")
	}
	_ = store.Create("j1", "book-toc", 3)
	_ = store.Create("j2", "payslip-ocr", 1)

	ts := httptest.NewServer(s)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/jobs?n=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Jobs       []JobResult `json:"jobs"`
		Persistent bool        `json:"persistent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if !body.Persistent {
		t.Error("persistent 应为 true")
	}
	if len(body.Jobs) != 2 {
		t.Fatalf("任务条数 = %d，期望 2", len(body.Jobs))
	}
	// 列表按创建时间倒序：j2 在前
	if body.Jobs[0].JobID != "j2" {
		t.Errorf("倒序不正确，首条 = %s", body.Jobs[0].JobID)
	}
	if body.Jobs[0].FinalReport != "" {
		t.Error("列表接口不应返回结果正文")
	}
}

func TestJobListEndpointWithoutPersistence(t *testing.T) {
	s := newStorageServer(t, false)
	ts := httptest.NewServer(s)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Jobs       []JobResult `json:"jobs"`
		Persistent bool        `json:"persistent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Persistent {
		t.Error("未开启持久化时 persistent 应为 false")
	}
	if len(body.Jobs) != 0 {
		t.Errorf("未开启持久化时列表应为空，得到 %d", len(body.Jobs))
	}
}

// decodeJobList 解析 /api/jobs 响应体。
func decodeJobList(t *testing.T, url string) ([]JobResult, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Jobs       []JobResult `json:"jobs"`
		Persistent bool        `json:"persistent"`
		Limit      int         `json:"limit"`
		Offset     int         `json:"offset"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	return body.Jobs, map[string]any{"persistent": body.Persistent, "limit": body.Limit, "offset": body.Offset}
}

func TestJobListEndpointFiltersAndPaging(t *testing.T) {
	s := newStorageServer(t, true)
	store := s.jobsStore()
	_ = store.Create("f1", "book-toc", 3)
	time.Sleep(time.Millisecond)
	_ = store.Create("f2", "payslip-ocr", 1)
	time.Sleep(time.Millisecond)
	_ = store.Create("f3", "book-toc", 2)
	_ = store.Update("f1", "failed", 0, 3, "", nil, "boom")

	ts := httptest.NewServer(s)
	defer ts.Close()

	// 按子功能过滤
	jobs, _ := decodeJobList(t, ts.URL+"/api/jobs?feature=book-toc")
	if len(jobs) != 2 {
		t.Fatalf("feature 过滤条数 = %d，期望 2", len(jobs))
	}
	for _, j := range jobs {
		if j.FeatureID != "book-toc" {
			t.Errorf("过滤结果含其它子功能: %s", j.FeatureID)
		}
	}
	// 按状态过滤
	jobs, _ = decodeJobList(t, ts.URL+"/api/jobs?status=failed")
	if len(jobs) != 1 || jobs[0].JobID != "f1" {
		t.Errorf("status 过滤 = %+v", jobs)
	}
	// 组合过滤（无匹配）
	jobs, _ = decodeJobList(t, ts.URL+"/api/jobs?feature=payslip-ocr&status=failed")
	if len(jobs) != 0 {
		t.Errorf("组合过滤应无结果，得到 %+v", jobs)
	}
	// 分页：n=2 取前两条（倒序 f3,f2），offset=2 取剩余
	jobs, meta := decodeJobList(t, ts.URL+"/api/jobs?n=2")
	if len(jobs) != 2 || jobs[0].JobID != "f3" || jobs[1].JobID != "f2" {
		t.Errorf("第一页 = %+v", jobs)
	}
	if meta["limit"] != 2 || meta["offset"] != 0 {
		t.Errorf("分页元信息 = %v", meta)
	}
	jobs, meta = decodeJobList(t, ts.URL+"/api/jobs?n=2&offset=2")
	if len(jobs) != 1 || jobs[0].JobID != "f1" {
		t.Errorf("第二页 = %+v", jobs)
	}
	if meta["offset"] != 2 {
		t.Errorf("offset 元信息 = %v", meta["offset"])
	}
}

func TestJobEventsStreamsTerminalEvent(t *testing.T) {
	s := newStorageServer(t, true)
	store := s.jobsStore()
	_ = store.Create("ev-1", "book-toc", 2)
	_ = store.Update("ev-1", "done", 2, 2, "完成", encodeJobResult(&nodes.State{FinalReport: "done-report"}), "")

	ts := httptest.NewServer(s)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/jobs/ev-1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "event: done") {
		t.Errorf("SSE 流应包含 done 事件，实际: %q", text)
	}
	if !strings.Contains(text, "done-report") {
		t.Errorf("done 事件应携带最终结果，实际: %q", text)
	}
}

func TestJobEventsStreamsFailedEvent(t *testing.T) {
	s := newStorageServer(t, true)
	store := s.jobsStore()
	_ = store.Create("ev-2", "book-toc", 2)
	_ = store.Update("ev-2", "failed", 0, 2, "", nil, "boom")

	ts := httptest.NewServer(s)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/jobs/ev-2/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "event: failed") {
		t.Errorf("SSE 流应包含 failed 事件，实际: %q", body)
	}
}

func TestJobEventsNotFound(t *testing.T) {
	s := newStorageServer(t, true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/jobs/absent/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("状态码 = %d，期望 404", resp.StatusCode)
	}
}

// ---- 启动期孤儿任务回收 ----

func TestFailInterruptedJobsOnStartup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "restart.db")
	// 第一个进程：留下一个 pending 任务后关闭
	first := &config.Config{
		LLM:     config.LLM{Providers: []config.Provider{{Name: "local", BaseURL: "http://localhost:1234/v1", APIKey: "k"}}},
		Storage: config.Storage{Enabled: true, DBPath: dbPath, PersistJobs: true},
	}
	s1, err := New(first)
	if err != nil {
		t.Fatalf("New#1: %v", err)
	}
	if err := s1.jobsStore().Create("orphan", "book-toc", 3); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	// 第二个进程：启动时应把残留任务标记为失败
	s2, err := New(first)
	if err != nil {
		t.Fatalf("New#2: %v", err)
	}
	defer s2.Close()
	row, err := s2.jobsStore().Get("orphan")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("孤儿任务应被标记为 failed，得到 %s", row.Status)
	}
	if !strings.Contains(row.Error, "中断") {
		t.Errorf("应写明中断原因，得到 %q", row.Error)
	}
	if _, ok := s2.lookupJob("orphan"); !ok {
		t.Error("重启后仍应能查到历史任务")
	}
}

func TestPruneOnceWithoutStorage(t *testing.T) {
	// recorder 为 nil 时清理应安全 no-op
	s, err := New(&config.Config{
		LLM: config.LLM{Providers: []config.Provider{{Name: "local", BaseURL: "http://x/v1", APIKey: "k"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.pruneOnce() // 不应 panic
}

func TestJobsStoreNilWhenDisabled(t *testing.T) {
	s := newStorageServer(t, false)
	if s.jobsStore() != nil {
		t.Error("未开启 persist-jobs 时 jobsStore 应为 nil")
	}
}

func TestLookupJobStorageErrorFallsBack(t *testing.T) {
	// 库中不存在但内存中存在（写入降级场景）时应回退内存
	s := newStorageServer(t, true)
	s.mu.Lock()
	s.rememberJobLocked(&JobResult{JobID: "degraded", Status: "done"})
	s.mu.Unlock()
	job, ok := s.lookupJob("degraded")
	if !ok {
		t.Fatal("库未命中时应回退内存查询")
	}
	if job.Status != "done" {
		t.Errorf("状态 = %q", job.Status)
	}
	// 两处都没有时才判定不存在
	if _, ok := s.lookupJob("nowhere"); ok {
		t.Error("两处都不存在时应返回 false")
	}
}

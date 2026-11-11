package api

import (
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
	job, err := s.startAsyncJob("not-registered", []string{"nonexistent.png"}, 2, nil)
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

func TestAsyncRunOverride(t *testing.T) {
	s := newStorageServer(t, false) // 全局未开持久化
	yes, no := true, false
	if !s.asyncRun("x", &yes) {
		t.Error("async=true 应强制异步（有存储时）")
	}
	if s.asyncRun("x", &no) {
		t.Error("async=false 应强制同步")
	}
	if s.asyncRun("x", nil) {
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

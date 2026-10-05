package stats

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openTestDB 在临时目录打开一个独立测试库（每个用例互不干扰）。
func openTestDB(t *testing.T) *Recorder {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "test.db"), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// TestOpenUpgradesLegacyCacheTable 验证旧库（llm_cache 缺 last_access_ms 列）
// 升级后能正常启动：补列必须发生在建索引之前，否则 CREATE INDEX 会报「列不存在」。
func TestOpenUpgradesLegacyCacheTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开旧库: %v", err)
	}
	_, err = legacy.Exec(`CREATE TABLE llm_cache (
		key TEXT PRIMARY KEY, provider TEXT, model TEXT, answer TEXT, reasoning TEXT,
		prompt_tokens INT, completion_tokens INT, created_at_ms INTEGER,
		expires_at_ms INTEGER, hit_count INT)`)
	if err != nil {
		t.Fatalf("建旧表: %v", err)
	}
	_ = legacy.Close()

	r, err := Open(path, 0)
	if err != nil {
		t.Fatalf("旧库应能升级后启动: %v", err)
	}
	defer r.Close()

	store := NewCacheStore(r)
	if err := store.CachePut("k", "p", "m", "a", "", 1, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CachePut: %v", err)
	}
	if _, _, _, _, _, _, ok := store.CacheGet("k"); !ok {
		t.Fatal("升级后缓存应可读写")
	}
	// 淘汰依赖 last_access_ms 列，能执行即说明补列成功
	if _, _, err := r.PruneCache(10); err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
}

// ---- 任务状态机 ----

func TestJobStoreLifecycle(t *testing.T) {
	store := openTestDB(t).Jobs()
	if store == nil {
		t.Fatal("Jobs() 不应返回 nil")
	}
	if err := store.Create("job-1", "payslip-ocr", 5); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 初始态：pending / progress=0 / total 保留
	row, err := store.Get("job-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Status != "pending" || row.Progress != 0 || row.Total != 5 {
		t.Errorf("初始态 = %s %d/%d", row.Status, row.Progress, row.Total)
	}
	if row.CreatedAt.IsZero() {
		t.Error("created_at 应已写入")
	}

	// 推进进度（result 为 nil 时应保留原值）
	if err := store.Update("job-1", "running", 2, 5, "ocr 2/5", nil, ""); err != nil {
		t.Fatalf("Update: %v", err)
	}
	row, _ = store.Get("job-1")
	if row.Status != "running" || row.Progress != 2 || row.Detail != "ocr 2/5" {
		t.Errorf("运行中态 = %s %d %q", row.Status, row.Progress, row.Detail)
	}
	if len(row.Result) != 0 {
		t.Error("未传 result 时不应写入结果")
	}

	// 终态：写入结果编码
	if err := store.Update("job-1", "done", 5, 5, "完成", []byte(`{"final_report":"ok"}`), ""); err != nil {
		t.Fatalf("Update done: %v", err)
	}
	row, _ = store.Get("job-1")
	if row.Status != "done" || row.Progress != 5 {
		t.Errorf("终态 = %s %d", row.Status, row.Progress)
	}
	if string(row.Result) != `{"final_report":"ok"}` {
		t.Errorf("结果 = %q", row.Result)
	}
	if !row.UpdatedAt.After(row.CreatedAt) && !row.UpdatedAt.Equal(row.CreatedAt) {
		t.Error("updated_at 不应早于 created_at")
	}
}

func TestJobStoreUpdateResultNilKeepsPrevious(t *testing.T) {
	store := openTestDB(t).Jobs()
	_ = store.Create("job-2", "book-toc", 3)
	_ = store.Update("job-2", "done", 3, 3, "", []byte(`{"a":1}`), "")
	// 纯状态更新（result=nil）不应把已有结果清空
	_ = store.Update("job-2", "done", 3, 3, "done", nil, "")
	row, _ := store.Get("job-2")
	if string(row.Result) != `{"a":1}` {
		t.Errorf("结果被覆盖为 %q", row.Result)
	}
}

func TestJobStoreGetNotFound(t *testing.T) {
	store := openTestDB(t).Jobs()
	if _, err := store.Get("missing"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("不存在的任务应返回 ErrJobNotFound，得到 %v", err)
	}
}

func TestJobStoreList(t *testing.T) {
	store := openTestDB(t).Jobs()
	for _, id := range []string{"j1", "j2", "j3"} {
		_ = store.Create(id, "book-toc", 1)
		time.Sleep(time.Millisecond) // 保证 created_at 有序
	}
	rows, err := store.List(0) // 0 → 默认 50
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("条数 = %d，期望 3", len(rows))
	}
	// 按创建时间倒序：最新在前
	if rows[0].ID != "j3" || rows[2].ID != "j1" {
		t.Errorf("倒序不正确: %s ... %s", rows[0].ID, rows[2].ID)
	}
	// 列表接口不带 result，避免把大字段带进响应
	if len(rows[0].Result) != 0 {
		t.Error("List 不应返回 result")
	}
	// n 超上限归一为 50（此处不影响条数）
	if _, err := store.List(9999); err != nil {
		t.Errorf("超大 n 应被归一，得到 %v", err)
	}
}

func TestJobStoreFailInterrupted(t *testing.T) {
	r := openTestDB(t)
	store := r.Jobs()
	_ = store.Create("pending-1", "f", 1)
	_ = store.Create("running-1", "f", 1)
	_ = store.Create("done-1", "f", 1)
	_ = store.Create("failed-1", "f", 1)
	_ = store.Update("running-1", "running", 1, 1, "", nil, "")
	_ = store.Update("done-1", "done", 1, 1, "", []byte(`{}`), "")
	_ = store.Update("failed-1", "failed", 0, 1, "", nil, "boom")

	n, err := store.FailInterrupted("服务重启，任务中断")
	if err != nil {
		t.Fatalf("FailInterrupted: %v", err)
	}
	if n != 2 { // 仅 pending / running
		t.Errorf("影响行数 = %d，期望 2", n)
	}
	for _, id := range []string{"pending-1", "running-1"} {
		row, _ := store.Get(id)
		if row.Status != "failed" {
			t.Errorf("%s 应被标记为 failed，得到 %s", id, row.Status)
		}
		if row.Error == "" {
			t.Errorf("%s 应写入中断原因", id)
		}
	}
	// 终态任务不受影响
	if row, _ := store.Get("done-1"); row.Status != "done" {
		t.Errorf("done 任务不应被改写，得到 %s", row.Status)
	}
	if row, _ := store.Get("failed-1"); row.Error != "boom" {
		t.Errorf("原失败原因不应被覆盖，得到 %q", row.Error)
	}
}

func TestJobStoreFailInterruptedEmptyMessage(t *testing.T) {
	store := openTestDB(t).Jobs()
	_ = store.Create("j", "f", 1)
	if _, err := store.FailInterrupted(""); err != nil {
		t.Fatal(err)
	}
	row, _ := store.Get("j")
	if row.Error == "" {
		t.Error("空消息时应回退默认提示")
	}
}

func TestJobStorePrune(t *testing.T) {
	r := openTestDB(t)
	store := r.Jobs()
	_ = store.Create("fresh", "f", 1)
	// 手工插入一条超期行
	old := time.Now().AddDate(0, 0, -40).UnixMilli()
	if _, err := r.DB().Exec(`INSERT INTO jobs (id, feature_id, status, progress, total, detail, result, error, created_at_ms, updated_at_ms)
		VALUES ('old','f','done',1,1,'',NULL,'',?,?)`, old, old); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(30); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := store.Get("old"); !errors.Is(err, ErrJobNotFound) {
		t.Error("超期任务应被清理")
	}
	if _, err := store.Get("fresh"); err != nil {
		t.Errorf("保留期内任务不应被清理: %v", err)
	}
	// 保留期 <=0 表示不清理
	if err := store.Prune(0); err != nil {
		t.Errorf("retentionDays<=0 应直接返回，得到 %v", err)
	}
}

func TestJobStoreQueryFiltersAndPaging(t *testing.T) {
	store := openTestDB(t).Jobs()
	_ = store.Create("a1", "book-toc", 3)
	time.Sleep(time.Millisecond)
	_ = store.Create("a2", "payslip-ocr", 1)
	time.Sleep(time.Millisecond)
	_ = store.Create("a3", "book-toc", 2)
	_ = store.Update("a3", "failed", 0, 2, "", nil, "boom")

	// 按子功能过滤
	rows, err := store.Query(JobFilter{Feature: "book-toc"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "a3" || rows[1].ID != "a1" {
		t.Errorf("按 feature 过滤 = %+v", rows)
	}
	// 按状态过滤
	rows, _ = store.Query(JobFilter{Status: "failed"})
	if len(rows) != 1 || rows[0].ID != "a3" {
		t.Errorf("按 status 过滤 = %+v", rows)
	}
	// 组合过滤（无匹配）
	rows, _ = store.Query(JobFilter{Feature: "payslip-ocr", Status: "failed"})
	if len(rows) != 0 {
		t.Errorf("组合过滤应无结果，得到 %+v", rows)
	}
	// 分页：倒序取前 2 条，再取跳过前 2 的
	page1, _ := store.Query(JobFilter{Limit: 2})
	if len(page1) != 2 || page1[0].ID != "a3" || page1[1].ID != "a2" {
		t.Errorf("第一页 = %+v", page1)
	}
	page2, _ := store.Query(JobFilter{Limit: 2, Offset: 2})
	if len(page2) != 1 || page2[0].ID != "a1" {
		t.Errorf("第二页 = %+v", page2)
	}
	// 非法参数归一
	if rows, err := store.Query(JobFilter{Limit: 9999, Offset: -5}); err != nil || len(rows) != 3 {
		t.Errorf("非法分页参数应被归一，得到 %d 条 / err=%v", len(rows), err)
	}
	// 不筛选时应返回全部
	all, _ := store.Query(JobFilter{})
	if len(all) != 3 {
		t.Errorf("无条件查询条数 = %d，期望 3", len(all))
	}
}

func TestJobStoreQueryNilSafety(t *testing.T) {
	var store *JobStore
	rows, err := store.Query(JobFilter{Limit: 10})
	if rows != nil || err != nil {
		t.Errorf("nil store Query 应返回 (nil, nil)，得到 (%v, %v)", rows, err)
	}
}

func TestJobStoreNilSafety(t *testing.T) {
	var store *JobStore
	if err := store.Create("x", "f", 1); err == nil {
		t.Error("nil store Create 应报错")
	}
	if _, err := store.Get("x"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("nil store Get 应返回 ErrJobNotFound，得到 %v", err)
	}
	if rows, err := store.List(10); rows != nil || err != nil {
		t.Errorf("nil store List 应返回 (nil, nil)，得到 (%v, %v)", rows, err)
	}
	if err := store.Prune(1); err != nil {
		t.Errorf("nil store Prune 应为 no-op，得到 %v", err)
	}
	if n, err := store.FailInterrupted("m"); n != 0 || err != nil {
		t.Errorf("nil store FailInterrupted 应为 (0, nil)，得到 (%d, %v)", n, err)
	}
	if err := store.Update("x", "done", 1, 1, "", nil, ""); err == nil {
		t.Error("nil store Update 应报错")
	}
}

// ---- model_caps ----

func TestCapsSaveLoadWarmAndExpire(t *testing.T) {
	r := openTestDB(t)
	c := CapsRow{Provider: "local", Model: "m1", Vision: true, Reasoning: false,
		Stream: true, StreamNote: "ok", ProbedVLM: true, ProbedJSON: true, HeuristicOK: true}
	if err := r.SaveCaps(c); err != nil {
		t.Fatalf("SaveCaps: %v", err)
	}

	got, ok := r.LoadCaps("local", "m1", 0)
	if !ok {
		t.Fatal("应能读回能力行")
	}
	// 未显式给 ProbedAtMS 时由 SaveCaps 回填（SaveCaps 按值接收，回填体现在库里）
	if got.ProbedAtMS == 0 {
		t.Error("SaveCaps 应回填 ProbedAtMS")
	}
	if got.Vision != true || got.Stream != true || got.StreamNote != "ok" || got.HeuristicOK != true {
		t.Errorf("读回内容不一致: %+v", got)
	}
	if _, ok := r.LoadCaps("local", "nope", 0); ok {
		t.Error("未探测的组合不应命中")
	}
	// 未过期的行应能被预热扫描到
	n := r.WarmCaps(time.Now().Add(-time.Hour).UnixMilli(), func(p, m string, row CapsRow) {
		if p != "local" || m != "m1" {
			t.Errorf("预热回调参数 = %q / %q", p, m)
		}
	})
	if n != 1 {
		t.Errorf("预热条数 = %d，期望 1", n)
	}
	// 以未来时间为下限：视为已过期
	if _, ok := r.LoadCaps("local", "m1", time.Now().Add(time.Hour).UnixMilli()); ok {
		t.Error("过期的能力行不应命中")
	}
	// 覆盖写入应更新而非新增
	c.Vision = false
	if err := r.SaveCaps(c); err != nil {
		t.Fatal(err)
	}
	got2, _ := r.LoadCaps("local", "m1", 0)
	if got2.Vision {
		t.Error("覆盖写入后 Vision 应为 false")
	}
	// 清理过期行
	if dropped, err := r.DropExpiredCaps(time.Duration(-1) * time.Hour); err != nil {
		t.Fatalf("DropExpiredCaps: %v", err)
	} else if dropped != 1 {
		t.Errorf("清理行数 = %d，期望 1", dropped)
	}
}

func TestCapsNilRecorder(t *testing.T) {
	var r *Recorder
	if _, ok := r.LoadCaps("p", "m", 0); ok {
		t.Error("nil Recorder 不应命中")
	}
	if err := r.SaveCaps(CapsRow{}); err == nil {
		t.Error("nil Recorder 保存应报错")
	}
	if n := r.WarmCaps(0, nil); n != 0 {
		t.Error("nil Recorder 预热应为 0")
	}
	if n, err := r.DropExpiredCaps(time.Hour); n != 0 || err != nil {
		t.Error("nil Recorder 清理应为 (0, nil)")
	}
}

// ---- llm_cache ----

func TestCacheStoreRoundTripAndExpiry(t *testing.T) {
	r := openTestDB(t)
	store := NewCacheStore(r)
	if err := store.CachePut("k1", "local", "m1", "answer", "think", 10, 20, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CachePut: %v", err)
	}
	p, m, a, reason, pt, ct, ok := store.CacheGet("k1")
	if !ok || p != "local" || m != "m1" || a != "answer" || reason != "think" || pt != 10 || ct != 20 {
		t.Errorf("round-trip 不一致: %q %q %q %q %d %d %v", p, m, a, reason, pt, ct, ok)
	}
	if _, _, _, _, _, _, ok := store.CacheGet("missing"); ok {
		t.Error("未写入的键不应命中")
	}
	// 过期条目不应命中
	_ = store.CachePut("k2", "local", "m1", "x", "", 0, 0, time.Now().Add(-time.Minute))
	if _, _, _, _, _, _, ok := store.CacheGet("k2"); ok {
		t.Error("过期条目不应命中")
	}
	// 命中计数累计（供概览展示收益）
	_, _, _, _, _, _, _ = store.CacheGet("k1")
	st, err := r.OverviewCache()
	if err != nil {
		t.Fatalf("OverviewCache: %v", err)
	}
	if st.Hits < 1 {
		t.Errorf("命中计数 = %d，期望 >=1", st.Hits)
	}
	if st.Rows < 1 {
		t.Errorf("缓存行数 = %d，期望 >=1", st.Rows)
	}
}

func TestCacheStorePrune(t *testing.T) {
	r := openTestDB(t)
	store := NewCacheStore(r)
	// 一条已过期
	_ = store.CachePut("expired", "p", "m", "a", "", 1, 1, time.Now().Add(-time.Hour))
	// 三条未过期（间隔写入，保证 created_at 有序）
	for _, k := range []string{"n1", "n2", "n3"} {
		_ = store.CachePut(k, "p", "m", "a", "", 1, 1, time.Now().Add(time.Hour))
		time.Sleep(2 * time.Millisecond)
	}
	dropped, trimmed, err := r.PruneCache(2)
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if dropped != 1 {
		t.Errorf("过期清理数 = %d，期望 1", dropped)
	}
	if trimmed != 1 {
		t.Errorf("容量淘汰数 = %d，期望 1（保留最新 2 条）", trimmed)
	}
	if _, _, _, _, _, _, ok := store.CacheGet("expired"); ok {
		t.Error("过期键应被清理")
	}
	// 淘汰的是最旧写入（n1），最新的两条应保留
	if _, _, _, _, _, _, ok := store.CacheGet("n1"); ok {
		t.Error("最旧的 n1 应被淘汰")
	}
	if _, _, _, _, _, _, ok := store.CacheGet("n3"); !ok {
		t.Error("最新的 n3 应保留")
	}
}

func TestCacheStoreEvictsLeastRecentlyUsed(t *testing.T) {
	r := openTestDB(t)
	store := NewCacheStore(r)
	// 依次写入三条：k1 最旧、k3 最新
	for _, k := range []string{"k1", "k2", "k3"} {
		_ = store.CachePut(k, "p", "m", "a", "", 1, 1, time.Now().Add(time.Hour))
		time.Sleep(2 * time.Millisecond)
	}
	// 命中 k1，使其成为「最近使用」——LRU 下它应被保留，未命中的 k2 才被淘汰
	if _, _, _, _, _, _, ok := store.CacheGet("k1"); !ok {
		t.Fatal("k1 应命中")
	}
	if _, trimmed, err := r.PruneCache(2); err != nil {
		t.Fatalf("PruneCache: %v", err)
	} else if trimmed != 1 {
		t.Errorf("淘汰数 = %d，期望 1", trimmed)
	}
	if _, _, _, _, _, _, ok := store.CacheGet("k1"); !ok {
		t.Error("刚命中过的 k1 应被保留（LRU）")
	}
	if _, _, _, _, _, _, ok := store.CacheGet("k2"); ok {
		t.Error("最久未使用的 k2 应被淘汰")
	}
	if _, _, _, _, _, _, ok := store.CacheGet("k3"); !ok {
		t.Error("k3 应被保留")
	}
}

func TestCacheGetRefreshesLastAccess(t *testing.T) {
	r := openTestDB(t)
	store := NewCacheStore(r)
	_ = store.CachePut("k", "p", "m", "a", "", 1, 1, time.Now().Add(time.Hour))
	e, ok := r.CacheGet("k")
	if !ok {
		t.Fatal("应命中")
	}
	if e.LastAccessMS <= 0 {
		t.Errorf("命中后应回填 last_access_ms，得到 %d", e.LastAccessMS)
	}
}

func TestCacheStoreNilSafety(t *testing.T) {
	var r *Recorder
	if e, ok := r.CacheGet("k"); ok || e.Key != "" {
		t.Error("nil Recorder 缓存读取应未命中")
	}
	if err := r.CachePut(CacheEntry{}, time.Time{}); err == nil {
		t.Error("nil Recorder 缓存写入应报错")
	}
	if d, tr, err := r.PruneCache(10); d != 0 || tr != 0 || err != nil {
		t.Error("nil Recorder 清理应为 (0,0,nil)")
	}
	var a *CacheStoreAdapter
	if _, _, _, _, _, _, ok := a.CacheGet("k"); ok {
		t.Error("nil 适配器不应命中")
	}
	if err := a.CachePut("k", "p", "m", "a", "", 0, 0, time.Time{}); err == nil {
		t.Error("nil 适配器写入应报错")
	}
}

func TestCapsKeyFormat(t *testing.T) {
	if got := CapsKey("p", "m"); got != "p|m" {
		t.Errorf("CapsKey = %q", got)
	}
}

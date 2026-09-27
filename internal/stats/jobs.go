// jobs.go 提供异步任务的 SQLite 仓储，以及与 calls 流水共用同一个库文件的
// 辅助能力建表 / 读写（model_caps 能力探测持久化、llm_cache 结果缓存）：
//
//	jobs(id, feature_id, status, progress, total, detail, result, error, created_at_ms, updated_at_ms)
//
// 写入 result/error 的编码与读取解码由调用方（api 包）负责，本包只做行读写，
// 因此不依赖任何业务类型，可独立测试。
package stats

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrJobNotFound 按 id 查询不到任务。
var ErrJobNotFound = errors.New("job not found")

// initAuxTables 建辅助表（幂等），旧库升级后首次启动自动补齐。
func initAuxTables(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY,
			feature_id TEXT,
			status TEXT,
			progress INT,
			total INT,
			detail TEXT,
			result BLOB,
			error TEXT,
			created_at_ms INTEGER,
			updated_at_ms INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_feature ON jobs(feature_id)`,
		`CREATE TABLE IF NOT EXISTS model_caps (
			key TEXT PRIMARY KEY,
			provider TEXT,
			model TEXT,
			vision BOOL,
			reasoning BOOL,
			stream BOOL,
			stream_note TEXT,
			probed_vlm BOOL,
			probed_json BOOL,
			heuristic_ok BOOL,
			probed_at_ms INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS llm_cache (
			key TEXT PRIMARY KEY,
			provider TEXT,
			model TEXT,
			answer TEXT,
			reasoning TEXT,
			prompt_tokens INT,
			completion_tokens INT,
			created_at_ms INTEGER,
			expires_at_ms INTEGER,
			hit_count INT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_llm_cache_expires ON llm_cache(expires_at_ms)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("init aux table: %w (sql: %s)", err, firstLine(q))
		}
	}
	return nil
}

// firstLine 取 SQL 首行（错误信息只保留表名线索，避免整段 SQL 刷屏）。
func firstLine(q string) string {
	for i := 0; i < len(q); i++ {
		if q[i] == '\n' {
			return q[:i]
		}
	}
	return q
}

// JobRow jobs 表的一行。
type JobRow struct {
	ID        string    `json:"job_id"`
	FeatureID string    `json:"feature_id"`
	Status    string    `json:"status"` // pending | running | done | failed
	Progress  int       `json:"progress"`
	Total     int       `json:"total"`
	Detail    string    `json:"detail,omitempty"`
	Result    []byte    `json:"-"` // 由调用方编码的 JSON
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// JobStore 异步任务仓储（基于统计库的同一个连接，延续单二进制/单文件理念）。
type JobStore struct {
	db *sql.DB
}

// Jobs 返回任务仓储；统计库未启用时返回 nil（各方法对 nil 接收者安全降级）。
func (r *Recorder) Jobs() *JobStore {
	if r == nil || r.db == nil {
		return nil
	}
	return &JobStore{db: r.db}
}

// NewJobStore 在已打开的数据库上构造任务仓储。
func NewJobStore(db *sql.DB) *JobStore { return &JobStore{db: db} }

// Create 登记一个新任务（status=pending），created_at/updated_at 取当前时间。
func (s *JobStore) Create(id, featureID string, total int) error {
	if s == nil || s.db == nil {
		return errors.New("job store 未启用")
	}
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO jobs
		(id, feature_id, status, progress, total, detail, result, error, created_at_ms, updated_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, featureID, "pending", 0, total, "", nil, "", now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return fmt.Errorf("insert job %s: %w", id, err)
	}
	return nil
}

// Update 写入任务的进度/状态/结果（结果与错误由调用方预先编码）。
// result 为 nil 时保留原值，便于纯进度更新不覆盖已完成的结果。
func (s *JobStore) Update(id, status string, progress, total int, detail string, result []byte, errMsg string) error {
	if s == nil || s.db == nil {
		return errors.New("job store 未启用")
	}
	_, err := s.db.Exec(`UPDATE jobs SET
		status = ?, progress = ?, total = ?, detail = ?,
		result = COALESCE(?, result), error = ?, updated_at_ms = ?
		WHERE id = ?`,
		status, progress, total, detail, result, errMsg, time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("update job %s: %w", id, err)
	}
	return nil
}

// Get 读取单个任务。不存在返回 ErrJobNotFound。
func (s *JobStore) Get(id string) (JobRow, error) {
	var row JobRow
	if s == nil || s.db == nil {
		return row, ErrJobNotFound
	}
	var (
		created, updated int64
		result           sql.NullString // result 为 BLOB/JSON（未写入时为 NULL）
	)
	err := s.db.QueryRow(`SELECT id, feature_id, status, progress, total,
			COALESCE(detail, ''), result, COALESCE(error, ''), created_at_ms, updated_at_ms
		FROM jobs WHERE id = ?`, id).
		Scan(&row.ID, &row.FeatureID, &row.Status, &row.Progress, &row.Total,
			&row.Detail, &result, &row.Error, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrJobNotFound
	}
	if err != nil {
		return row, fmt.Errorf("query job %s: %w", id, err)
	}
	if result.Valid {
		row.Result = []byte(result.String)
	}
	row.CreatedAt = msTime(created)
	row.UpdatedAt = msTime(updated)
	return row, nil
}

// List 返回最近 n 条任务（不含 result，避免把大字段带进列表接口）。n<=0 取 50。
func (s *JobStore) List(n int) ([]JobRow, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if n <= 0 || n > 200 {
		n = 50
	}
	rows, err := s.db.Query(`SELECT id, feature_id, status, progress, total,
			COALESCE(detail, ''), COALESCE(error, ''), created_at_ms, updated_at_ms
		FROM jobs ORDER BY created_at_ms DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	out := make([]JobRow, 0, n)
	for rows.Next() {
		var (
			row              JobRow
			created, updated int64
		)
		if err := rows.Scan(&row.ID, &row.FeatureID, &row.Status, &row.Progress, &row.Total,
			&row.Detail, &row.Error, &created, &updated); err != nil {
			return nil, err
		}
		row.CreatedAt = msTime(created)
		row.UpdatedAt = msTime(updated)
		out = append(out, row)
	}
	return out, rows.Err()
}

// Prune 删除早于保留期的任务行（与 calls 流水共用 retention-days）。
func (s *JobStore) Prune(retentionDays int) error {
	if s == nil || s.db == nil || retentionDays <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM jobs WHERE created_at_ms < ?`, cutoff); err != nil {
		return fmt.Errorf("prune jobs: %w", err)
	}
	return nil
}

// FailInterrupted 启动期把上次进程遗留的 pending/running 任务标为失败
// （进程已死，没人会再更新它们；不处理则前端永久轮询）。返回影响行数。
func (s *JobStore) FailInterrupted(msg string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	if msg == "" {
		msg = "服务重启，任务中断"
	}
	res, err := s.db.Exec(`UPDATE jobs SET status = 'failed', error = ?, updated_at_ms = ?
		WHERE status IN ('pending', 'running')`, msg, time.Now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("fail interrupted jobs: %w", err)
	}
	return res.RowsAffected()
}

// msTime 毫秒时间戳转 time（0 视为零值）。
func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// ---- model_caps：模型能力探测结果持久化（重启免重测，带 TTL）----

// CapsRow model_caps 表的一行。
type CapsRow struct {
	Provider    string
	Model       string
	Vision      bool
	Reasoning   bool
	Stream      bool
	StreamNote  string
	ProbedVLM   bool
	ProbedJSON  bool
	HeuristicOK bool
	ProbedAtMS  int64
}

// CapsKey 能力缓存主键（与 api 层进程内缓存保持一致的拼法）。
func CapsKey(provider, model string) string { return provider + "|" + model }

// LoadCaps 读取未过期的能力探测结果。minProbedAtMS<=0 表示忽略过期判断。
func (r *Recorder) LoadCaps(provider, model string, minProbedAtMS int64) (CapsRow, bool) {
	var out CapsRow
	if r == nil || r.db == nil {
		return out, false
	}
	q := `SELECT provider, model, vision, reasoning, stream, COALESCE(stream_note, ''),
			probed_vlm, probed_json, heuristic_ok, probed_at_ms
		FROM model_caps WHERE key = ?`
	args := []any{CapsKey(provider, model)}
	if minProbedAtMS > 0 {
		q += ` AND probed_at_ms >= ?`
		args = append(args, minProbedAtMS)
	}
	err := r.db.QueryRow(q, args...).Scan(
		&out.Provider, &out.Model, &out.Vision, &out.Reasoning, &out.Stream, &out.StreamNote,
		&out.ProbedVLM, &out.ProbedJSON, &out.HeuristicOK, &out.ProbedAtMS)
	if err != nil {
		return out, false
	}
	return out, true
}

// SaveCaps 写入/覆盖能力探测结果。
func (r *Recorder) SaveCaps(c CapsRow) error {
	if r == nil || r.db == nil {
		return errors.New("stats 未启用，能力探测结果无法持久化")
	}
	if c.ProbedAtMS == 0 {
		c.ProbedAtMS = time.Now().UnixMilli()
	}
	_, err := r.db.Exec(`INSERT INTO model_caps
		(key, provider, model, vision, reasoning, stream, stream_note,
		 probed_vlm, probed_json, heuristic_ok, probed_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET
			vision=excluded.vision, reasoning=excluded.reasoning, stream=excluded.stream,
			stream_note=excluded.stream_note, probed_vlm=excluded.probed_vlm,
			probed_json=excluded.probed_json, heuristic_ok=excluded.heuristic_ok,
			probed_at_ms=excluded.probed_at_ms`,
		CapsKey(c.Provider, c.Model), c.Provider, c.Model, c.Vision, c.Reasoning, c.Stream,
		c.StreamNote, c.ProbedVLM, c.ProbedJSON, c.HeuristicOK, c.ProbedAtMS)
	if err != nil {
		return fmt.Errorf("save caps: %w", err)
	}
	return nil
}

// WarmCaps 扫描未过期的能力探测行，逐条交给 fill（启动预热用），返回加载条数。
func (r *Recorder) WarmCaps(minProbedAtMS int64, fill func(provider, model string, c CapsRow)) int {
	if r == nil || r.db == nil || fill == nil {
		return 0
	}
	rows, err := r.db.Query(`SELECT provider, model, vision, reasoning, stream, COALESCE(stream_note, ''),
			probed_vlm, probed_json, heuristic_ok, probed_at_ms
		FROM model_caps WHERE probed_at_ms >= ?`, minProbedAtMS)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var c CapsRow
		if err := rows.Scan(&c.Provider, &c.Model, &c.Vision, &c.Reasoning, &c.Stream, &c.StreamNote,
			&c.ProbedVLM, &c.ProbedJSON, &c.HeuristicOK, &c.ProbedAtMS); err != nil {
			continue
		}
		fill(c.Provider, c.Model, c)
		n++
	}
	return n
}

// DropExpiredCaps 删除超过 TTL 的能力探测行，返回删除数量。
func (r *Recorder) DropExpiredCaps(ttl time.Duration) (int64, error) {
	if r == nil || r.db == nil {
		return 0, nil
	}
	res, err := r.db.Exec(`DELETE FROM model_caps WHERE probed_at_ms < ?`,
		time.Now().Add(-ttl).UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("drop expired caps: %w", err)
	}
	return res.RowsAffected()
}

// ---- llm_cache：LLM 结果缓存（相同输入命中即免重复烧 token）----

// CacheEntry llm_cache 表的一行。
type CacheEntry struct {
	Key              string
	Provider         string
	Model            string
	Answer           string
	Reasoning        string
	PromptTokens     int
	CompletionTokens int
	ExpiresAtMS      int64
}

// CacheGet 按 key 命中未过期的缓存回答（命中计数 +1，供概览展示收益）。
func (r *Recorder) CacheGet(key string) (CacheEntry, bool) {
	var out CacheEntry
	if r == nil || r.db == nil {
		return out, false
	}
	err := r.db.QueryRow(`SELECT key, provider, model, answer, COALESCE(reasoning, ''),
			prompt_tokens, completion_tokens, expires_at_ms
		FROM llm_cache WHERE key = ? AND (expires_at_ms <= 0 OR expires_at_ms > ?)`,
		key, time.Now().UnixMilli()).
		Scan(&out.Key, &out.Provider, &out.Model, &out.Answer, &out.Reasoning,
			&out.PromptTokens, &out.CompletionTokens, &out.ExpiresAtMS)
	if err != nil {
		return out, false
	}
	_, _ = r.db.Exec(`UPDATE llm_cache SET hit_count = COALESCE(hit_count, 0) + 1 WHERE key = ?`, key)
	return out, true
}

// CachePut 写入/覆盖一条缓存回答（expiresAt 零值表示不过期）。
func (r *Recorder) CachePut(e CacheEntry, expiresAt time.Time) error {
	if r == nil || r.db == nil {
		return errors.New("stats 未启用，结果缓存无法写入")
	}
	var expiresMS int64
	if !expiresAt.IsZero() {
		expiresMS = expiresAt.UnixMilli()
	}
	_, err := r.db.Exec(`INSERT INTO llm_cache
		(key, provider, model, answer, reasoning, prompt_tokens, completion_tokens,
		 created_at_ms, expires_at_ms, hit_count)
		VALUES (?,?,?,?,?,?,?,?,?,0)
		ON CONFLICT(key) DO UPDATE SET
			answer=excluded.answer, reasoning=excluded.reasoning,
			prompt_tokens=excluded.prompt_tokens, completion_tokens=excluded.completion_tokens,
			created_at_ms=excluded.created_at_ms, expires_at_ms=excluded.expires_at_ms`,
		e.Key, e.Provider, e.Model, e.Answer, e.Reasoning, e.PromptTokens, e.CompletionTokens,
		time.Now().UnixMilli(), expiresMS)
	if err != nil {
		return fmt.Errorf("cache put: %w", err)
	}
	return nil
}

// CacheStoreAdapter llm.CacheStore 接口实现（独立类型，避免 Recorder 方法集膨胀）。
type CacheStoreAdapter struct{ r *Recorder }

// NewCacheStore 返回供 llm 层注入的缓存仓储适配器；recorder 为 nil 时返回 nil。
func NewCacheStore(r *Recorder) *CacheStoreAdapter {
	if r == nil {
		return nil
	}
	return &CacheStoreAdapter{r: r}
}

// CacheGet 按 key 取回缓存回答。
func (a *CacheStoreAdapter) CacheGet(key string) (provider, model, answer, reasoning string, promptTokens, completionTokens int, ok bool) {
	if a == nil {
		return "", "", "", "", 0, 0, false
	}
	e, ok := a.r.CacheGet(key)
	if !ok {
		return "", "", "", "", 0, 0, false
	}
	return e.Provider, e.Model, e.Answer, e.Reasoning, e.PromptTokens, e.CompletionTokens, true
}

// CachePut 写入一条缓存回答。
func (a *CacheStoreAdapter) CachePut(key, provider, model, answer, reasoning string,
	promptTokens, completionTokens int, expiresAt time.Time) error {
	if a == nil {
		return errors.New("stats 未启用，结果缓存无法写入")
	}
	return a.r.CachePut(CacheEntry{
		Key: key, Provider: provider, Model: model,
		Answer: answer, Reasoning: reasoning,
		PromptTokens: promptTokens, CompletionTokens: completionTokens,
	}, expiresAt)
}

// PruneCache 清理过期缓存行并把行数压到 maxRows 以内（淘汰最旧写入）。
func (r *Recorder) PruneCache(maxRows int) (dropped, trimmed int64, err error) {
	if r == nil || r.db == nil {
		return 0, 0, nil
	}
	res, err := r.db.Exec(`DELETE FROM llm_cache WHERE expires_at_ms > 0 AND expires_at_ms < ?`,
		time.Now().UnixMilli())
	if err != nil {
		return 0, 0, fmt.Errorf("prune expired cache: %w", err)
	}
	dropped, _ = res.RowsAffected()
	if maxRows <= 0 {
		return dropped, 0, nil
	}
	res, err = r.db.Exec(`DELETE FROM llm_cache WHERE key IN (
		SELECT key FROM llm_cache ORDER BY created_at_ms DESC LIMIT -1 OFFSET ?)`, maxRows)
	if err != nil {
		return dropped, 0, fmt.Errorf("trim cache: %w", err)
	}
	trimmed, _ = res.RowsAffected()
	return dropped, trimmed, nil
}

// CacheStats 缓存规模与命中概览（写入 /api/overview）。
type CacheStats struct {
	Rows int64 `json:"rows"`
	Hits int64 `json:"hits"`
}

// OverviewCache 汇总缓存命中数据；库未启用时返回零值。
func (r *Recorder) OverviewCache() (CacheStats, error) {
	var s CacheStats
	if r == nil || r.db == nil {
		return s, nil
	}
	err := r.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(hit_count), 0) FROM llm_cache`).
		Scan(&s.Rows, &s.Hits)
	if err != nil {
		return s, fmt.Errorf("overview cache stats: %w", err)
	}
	return s, nil
}

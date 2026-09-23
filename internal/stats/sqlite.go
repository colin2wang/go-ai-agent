// Package stats 提供 LLM 调用统计的 SQLite 落库、保留期清理与概览聚合查询。
package stats

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Recorder 单例封装 *sql.DB，异步队列经 llm.StatsLogger 接口调用。
type Recorder struct {
	db *sql.DB
}

// CallRow 单次调用流水（概览页「最近调用」）。
type CallRow struct {
	TS               string `json:"ts"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	LatencyMS        int    `json:"latency_ms"`
	OK               bool   `json:"ok"`
	Err              string `json:"err"`
}

// DayPoint 按天的调用聚合（趋势图）。
type DayPoint struct {
	Day      string `json:"day"` // MM-DD
	Calls    int64  `json:"calls"`
	Tokens   int64  `json:"tokens"`
	Failures int64  `json:"failures"`
}

// ModelStat 按 provider+model 的用量聚合。
type ModelStat struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Calls        int64  `json:"calls"`
	Tokens       int64  `json:"tokens"`
	AvgLatencyMS int64  `json:"avg_latency_ms"`
	Failures     int64  `json:"failures"`
}

// Overview 概览页所需的全部统计聚合。
type Overview struct {
	TotalCalls   int64       `json:"total_calls"`
	TodayCalls   int64       `json:"today_calls"`
	TotalTokens  int64       `json:"total_tokens"`
	AvgLatencyMS int64       `json:"avg_latency_ms"`
	SuccessRate  float64     `json:"success_rate"` // 0~1
	LastCallAt   string      `json:"last_call_at"`
	Daily        []DayPoint  `json:"daily"`
	Models       []ModelStat `json:"models"`
	Recent       []CallRow   `json:"recent"`
}

// Open 打开（必要时自动创建）数据库并建表，清理超过保留期的流水。
func Open(path string, retentionDays int) (*Recorder, error) {
	// db-path 所在目录不存在则自动创建（如 ./database/）
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// 并发写锁绕行：单连接 + WAL 模式
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("set wal: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts DATETIME,
		ts_ms INTEGER,
		provider TEXT,
		model TEXT,
		prompt_tokens INT,
		completion_tokens INT,
		latency_ms INT,
		ok BOOL,
		err TEXT
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table calls: %w", err)
	}
	// 兼容旧库：补齐 ts_ms 列与索引（已存在则忽略错误）
	_, _ = db.Exec(`ALTER TABLE calls ADD COLUMN ts_ms INTEGER`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_calls_ts_ms ON calls(ts_ms)`)

	r := &Recorder{db: db}
	if retentionDays > 0 {
		if err := r.Cleanup(retentionDays); err != nil {
			db.Close()
			return nil, err
		}
	}
	return r, nil
}

// Log 写入一条调用流水（实现 llm.StatsLogger）。
func (r *Recorder) Log(provider, model string, promptTokens, completionTokens, latencyMs int, ok bool, errMsg string) {
	if r == nil {
		return
	}
	now := time.Now()
	_, _ = r.db.Exec(`INSERT INTO calls
		(ts, ts_ms, provider, model, prompt_tokens, completion_tokens, latency_ms, ok, err)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		now, now.UnixMilli(), provider, model, promptTokens, completionTokens, latencyMs, ok, errMsg)
}

// Cleanup 删除超过保留期的流水。
// 兼容旧库：ts_ms 为 NULL 的历史行一并视为过期清理。
func (r *Recorder) Cleanup(retentionDays int) error {
	cutoff := time.Now().AddDate(0, 0, -retentionDays).UnixMilli()
	if _, err := r.db.Exec(`DELETE FROM calls WHERE COALESCE(ts_ms, 0) < ?`, cutoff); err != nil {
		return fmt.Errorf("cleanup calls: %w", err)
	}
	return nil
}

// Close 关闭数据库。
func (r *Recorder) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

// Overview 聚合最近 days 天的调用指标（days<=0 时取 14）。
func (r *Recorder) Overview(days int) (*Overview, error) {
	ov := &Overview{Daily: []DayPoint{}, Models: []ModelStat{}, Recent: []CallRow{}}
	if r == nil || r.db == nil {
		return ov, nil
	}
	if days <= 0 {
		days = 14
	}
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	sinceMs := todayStart.AddDate(0, 0, -(days - 1)).UnixMilli()
	todayMs := todayStart.UnixMilli()

	// ---- 区间汇总 ----
	var (
		failures int64
		avgMs    float64
		lastMS   int64
	)
	row := r.db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(prompt_tokens + completion_tokens), 0),
			COALESCE(AVG(latency_ms), 0),
			COALESCE(SUM(CASE WHEN ok THEN 0 ELSE 1 END), 0),
			COALESCE(MAX(ts_ms), 0)
		FROM calls WHERE ts_ms >= ?`, sinceMs)
	if err := row.Scan(&ov.TotalCalls, &ov.TotalTokens, &avgMs, &failures, &lastMS); err != nil {
		return nil, fmt.Errorf("overview totals: %w", err)
	}
	ov.AvgLatencyMS = int64(avgMs)
	if ov.TotalCalls > 0 {
		ov.SuccessRate = float64(ov.TotalCalls-failures) / float64(ov.TotalCalls)
	}
	if lastMS > 0 {
		ov.LastCallAt = time.UnixMilli(lastMS).Format("2006-01-02 15:04:05")
	}

	// ---- 今日调用 ----
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE ts_ms >= ?`, todayMs).Scan(&ov.TodayCalls); err != nil {
		return nil, fmt.Errorf("overview today: %w", err)
	}

	// ---- 按天趋势 ----
	rows, err := r.db.Query(`SELECT strftime('%Y-%m-%d', ts_ms / 1000, 'unixepoch', 'localtime') AS d,
			COUNT(*),
			COALESCE(SUM(prompt_tokens + completion_tokens), 0),
			COALESCE(SUM(CASE WHEN ok THEN 0 ELSE 1 END), 0)
		FROM calls WHERE ts_ms >= ?
		GROUP BY d ORDER BY d`, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("overview daily: %w", err)
	}
	byDay := map[string]DayPoint{}
	for rows.Next() {
		var p DayPoint
		if err := rows.Scan(&p.Day, &p.Calls, &p.Tokens, &p.Failures); err != nil {
			rows.Close()
			return nil, err
		}
		byDay[p.Day] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 补齐空白天，保证趋势图 x 轴连续
	for i := 0; i < days; i++ {
		d := todayStart.AddDate(0, 0, -(days - 1 - i))
		key := d.Format("2006-01-02")
		if p, ok := byDay[key]; ok {
			ov.Daily = append(ov.Daily, p)
		} else {
			ov.Daily = append(ov.Daily, DayPoint{Day: key})
		}
	}

	// ---- 模型用量 Top N ----
	mrows, err := r.db.Query(`SELECT provider, model,
			COUNT(*),
			COALESCE(SUM(prompt_tokens + completion_tokens), 0),
			COALESCE(AVG(latency_ms), 0),
			COALESCE(SUM(CASE WHEN ok THEN 0 ELSE 1 END), 0)
		FROM calls WHERE ts_ms >= ?
		GROUP BY provider, model ORDER BY COUNT(*) DESC LIMIT 8`, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("overview models: %w", err)
	}
	for mrows.Next() {
		var m ModelStat
		var avg float64
		if err := mrows.Scan(&m.Provider, &m.Model, &m.Calls, &m.Tokens, &avg, &m.Failures); err != nil {
			mrows.Close()
			return nil, err
		}
		m.AvgLatencyMS = int64(avg)
		ov.Models = append(ov.Models, m)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return nil, err
	}

	// ---- 最近调用 ----
	rrows, err := r.db.Query(`SELECT ts_ms, provider, model, prompt_tokens, completion_tokens, latency_ms, ok, COALESCE(err, '')
		FROM calls ORDER BY id DESC LIMIT 12`)
	if err != nil {
		return nil, fmt.Errorf("overview recent: %w", err)
	}
	for rrows.Next() {
		var (
			ms int64
			c  CallRow
		)
		if err := rrows.Scan(&ms, &c.Provider, &c.Model, &c.PromptTokens, &c.CompletionTokens,
			&c.LatencyMS, &c.OK, &c.Err); err != nil {
			rrows.Close()
			return nil, err
		}
		if ms > 0 {
			c.TS = time.UnixMilli(ms).Format("2006-01-02 15:04:05")
		}
		ov.Recent = append(ov.Recent, c)
	}
	rrows.Close()
	return ov, rrows.Err()
}

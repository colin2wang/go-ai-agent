// Package stats 提供 llm-stats 的 SQLite 落库（纯 Go，无 CGO）与保留期清理。
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
	_, _ = r.db.Exec(`INSERT INTO calls VALUES (NULL,?,?,?,?,?,?,?,?)`,
		time.Now(), provider, model, promptTokens, completionTokens, latencyMs, ok, errMsg)
}

// Cleanup 删除超过保留期的流水，返回删除行数。
func (r *Recorder) Cleanup(retentionDays int) error {
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	res, err := r.db.Exec(`DELETE FROM calls WHERE ts < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("cleanup calls: %w", err)
	}
	n, _ := res.RowsAffected()
	_ = n
	return nil
}

// Close 关闭数据库。
func (r *Recorder) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

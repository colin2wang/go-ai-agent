package nodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// ---- 可重试错误判定 ----

func TestErrorCode(t *testing.T) {
	cases := []struct {
		msg  string
		want int
	}{
		{"error, status code: 429, message: rate limit", http.StatusTooManyRequests},
		{"error, status code: 500", http.StatusInternalServerError},
		{"status code: 503", http.StatusServiceUnavailable},
		{"status code:404", http.StatusNotFound},
		{"statuscode: 404", 0}, // 缺空格不匹配
		{"no status here", 0},
		{"status code: abc", 0},
	}
	for _, tc := range cases {
		if got := errorCode(tc.msg); got != tc.want {
			t.Errorf("errorCode(%q) = %d，期望 %d", tc.msg, got, tc.want)
		}
	}
}

func TestRetryableErrClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"限流 429", fmt.Errorf("error, status code: 429"), true},
		{"服务端 500", fmt.Errorf("error, status code: 500"), true},
		{"网关 502", fmt.Errorf("error, status code: 502"), true},
		{"请求超时 408", fmt.Errorf("error, status code: 408"), true},
		{"参数错误 400", fmt.Errorf("error, status code: 400"), false},
		{"未授权 401", fmt.Errorf("error, status code: 401"), false},
		{"不存在 404", fmt.Errorf("error, status code: 404"), false},
		{"无状态码的连接拒绝", errors.New("dial tcp: connection refused"), true},
		{"无状态码的读超时", errors.New("read tcp: i/o timeout"), true},
		{"无状态码的 EOF", errors.New("unexpected EOF"), true},
		{"无状态码的未知主机", errors.New("no such host"), true},
		{"无状态码的普通错误", errors.New("something went wrong"), false},
		{"调用方主动取消", context.Canceled, false},
		{"调用方超时", context.DeadlineExceeded, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableErr(tc.err); got != tc.want {
				t.Errorf("retryableErr(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// ---- Retry-After 解析 ----

func TestRetryAfterFromErr(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		want   time.Duration
		wantOK bool
	}{
		{"无提示", errors.New("error, status code: 429"), 0, false},
		{"nil", nil, 0, false},
		{"标准秒", errors.New("rate limited, Retry-After: 30"), 30 * time.Second, true},
		{"小写带连字符", errors.New("retry-after: 5"), 5 * time.Second, true},
		{"毫秒", errors.New("please retry after 1500ms"), 1500 * time.Millisecond, true},
		{"JSON 正文", errors.New(`{"error":{"message":"slow down, retry after 3s"}}`), 3 * time.Second, true},
		{"超上限被截断", errors.New("Retry-After: 7200"), maxRetryAfter, true},
		{"非法数值", errors.New("Retry-After: abc"), 0, false},
		{"零值", errors.New("Retry-After: 0"), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := retryAfterFromErr(tc.err)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v，期望 %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("时长 = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// ---- 退避时长 ----

func TestBackoffDelayExponential(t *testing.T) {
	base := 100
	for attempt, want := range []time.Duration{100, 200, 400, 800, 1600, 3200} {
		if got := backoffDelay(base, attempt, errors.New("boom")); got != want*time.Millisecond {
			t.Errorf("attempt=%d: 时长 = %v，期望 %v", attempt, got, want*time.Millisecond)
		}
	}
	// 指数增长在上限处封顶（attempt 再大也不超过 maxBackoff）
	if got := backoffDelay(60_000, 10, errors.New("boom")); got != maxBackoff {
		t.Errorf("超长退避应封顶为 %v，得到 %v", maxBackoff, got)
	}
	// base<=0 且无 Retry-After 时不等待
	if got := backoffDelay(0, 3, errors.New("boom")); got != 0 {
		t.Errorf("base<=0 时应为 0，得到 %v", got)
	}
}

func TestBackoffDelayPrefersRetryAfter(t *testing.T) {
	// 端点明确指示 2s，即使指数退避只算出 100ms 也应遵循端点
	err := errors.New("error, status code: 429, Retry-After: 2")
	if got := backoffDelay(100, 0, err); got != 2*time.Second {
		t.Errorf("应遵循 Retry-After，得到 %v", got)
	}
}

func TestSleepBackoffHonorsContext(t *testing.T) {
	// 已取消的 ctx：等待时长 >0 时应立即返回 false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepBackoff(ctx, 10_000, 0, errors.New("429")) {
		t.Error("ctx 已取消时应返回 false")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("应立即返回，实际耗时 %v", elapsed)
	}
}

func TestSleepBackoffZeroWait(t *testing.T) {
	// 可重试但无退避基数：不挂起，直接返回 true（保持「立即重试」行为）
	start := time.Now()
	if !sleepBackoff(context.Background(), 0, 0, errors.New("boom")) {
		t.Error("base<=0 时应返回 true（立即重试）")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("不应挂起，实际耗时 %v", elapsed)
	}
}

// ---- 重试循环 ----

// scriptedCalls 按脚本依次返回预设结果；用完最后一个则重复返回它。
type scriptedCalls struct {
	results []error
	okText  string
	n       int
}

func (s *scriptedCalls) next() (string, error) {
	i := s.n
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	s.n++
	if err := s.results[i]; err != nil {
		return "", err
	}
	return s.okText, nil
}

func TestRetryLoopSucceedsAfterTransientErrors(t *testing.T) {
	s := &scriptedCalls{
		results: []error{
			fmt.Errorf("error, status code: 429"),
			fmt.Errorf("error, status code: 500"),
			nil, // 第三次成功
		},
		okText: "final-answer",
	}
	// Max=2 允许两次额外尝试；退避基数设为 0 让测试不必真等
	got, err := retryLoop(context.Background(), retryPolicy{Max: 2, BackoffMS: 0}, "ocr", s.next)
	if err != nil {
		t.Fatalf("两次可重试错误后应成功，得到 %v", err)
	}
	if got != "final-answer" {
		t.Errorf("结果 = %q", got)
	}
	if s.n != 3 {
		t.Errorf("调用次数 = %d，期望 3", s.n)
	}
}

func TestRetryLoopStopsOnNonRetryable(t *testing.T) {
	s := &scriptedCalls{results: []error{fmt.Errorf("error, status code: 400 bad request")}}
	_, err := retryLoop(context.Background(), retryPolicy{Max: 3, BackoffMS: 0}, "ocr", s.next)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if s.n != 1 {
		t.Errorf("4xx 不应重试，调用次数 = %d，期望 1", s.n)
	}
}

func TestRetryLoopExhaustsAttempts(t *testing.T) {
	s := &scriptedCalls{results: []error{fmt.Errorf("error, status code: 503")}}
	_, err := retryLoop(context.Background(), retryPolicy{Max: 2, BackoffMS: 0}, "ocr", s.next)
	if err == nil {
		t.Fatal("重试耗尽后应返回错误")
	}
	if s.n != 3 { // 首次 + 2 次额外尝试
		t.Errorf("调用次数 = %d，期望 3（首次 + Max=2）", s.n)
	}
}

func TestRetryLoopNoRetryByDefault(t *testing.T) {
	s := &scriptedCalls{results: []error{fmt.Errorf("error, status code: 500")}}
	_, err := retryLoop(context.Background(), retryPolicy{BackoffMS: 0}, "ocr", s.next)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if s.n != 1 {
		t.Errorf("Max=0 时不应重试，调用次数 = %d", s.n)
	}
}

func TestRetryLoopNegativeMaxNormalized(t *testing.T) {
	s := &scriptedCalls{results: []error{fmt.Errorf("error, status code: 500")}}
	_, _ = retryLoop(context.Background(), retryPolicy{Max: -5, BackoffMS: 0}, "ocr", s.next)
	if s.n != 1 {
		t.Errorf("负 Max 应归一为 0，调用次数 = %d", s.n)
	}
}

func TestRetryLoopContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &scriptedCalls{results: []error{fmt.Errorf("error, status code: 500")}}
	start := time.Now()
	if _, err := retryLoop(ctx, retryPolicy{Max: 5, BackoffMS: 0}, "ocr", s.next); err == nil {
		t.Fatal("ctx 取消时应返回错误")
	}
	if s.n != 1 {
		t.Errorf("ctx 取消后不应继续重试，调用次数 = %d", s.n)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("应立即返回，实际耗时 %v", elapsed)
	}
}

func TestRetryLoopFirstTrySuccess(t *testing.T) {
	s := &scriptedCalls{results: []error{nil}, okText: "ok"}
	got, err := retryLoop(context.Background(), retryPolicy{Max: 3}, "ocr", s.next)
	if err != nil || got != "ok" {
		t.Fatalf("首次成功应直接返回，得到 %q / %v", got, err)
	}
	if s.n != 1 {
		t.Errorf("调用次数 = %d，期望 1", s.n)
	}
}

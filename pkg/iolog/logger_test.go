package iolog

import (
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"

	"go-ai-agent/internal/config"
)

func TestTruncateShortTextUnchanged(t *testing.T) {
	// 长度 <= head+tail+20 时原样返回
	s := strings.Repeat("a", 40)
	if got := Truncate(s, 10, 10); got != s {
		t.Errorf("短文本不应截断，得到 %q", got)
	}
	// 恰好等于阈值
	s = strings.Repeat("b", 50)
	if got := Truncate(s, 10, 20); got != s {
		t.Errorf("等于阈值不应截断，得到 %q", got)
	}
}

func TestTruncateLongText(t *testing.T) {
	s := strings.Repeat("x", 100)
	got := Truncate(s, 10, 8)
	want := strings.Repeat("x", 10) + "...[省略 82 字符]..." + strings.Repeat("x", 8)
	if got != want {
		t.Errorf("Truncate = %q, want %q", got, want)
	}
	// 省略数 = 总长 - head - tail
	if !strings.Contains(got, "省略 82 字符") {
		t.Errorf("省略计数错误: %q", got)
	}
}

func TestTruncateNegativeLimits(t *testing.T) {
	s := strings.Repeat("y", 100)
	got := Truncate(s, -5, -3)
	// head/tail 归零后仅保留省略标注
	want := "...[省略 100 字符]..."
	if got != want {
		t.Errorf("负数参数应归零，得到 %q, want %q", got, want)
	}
}

func TestTruncateChineseRunes(t *testing.T) {
	// 按 rune 截断，中文字符不被切碎
	s := strings.Repeat("汉", 100)
	got := Truncate(s, 5, 5)
	if got != "汉汉汉汉汉...[省略 90 字符]...汉汉汉汉汉" {
		t.Errorf("中文按 rune 截断错误: %q", got)
	}
}

func TestOneline(t *testing.T) {
	cases := map[string]string{
		"a\nb":       "a b",
		"a\tb":       "a b",
		"a  b":       "a b",
		"a \n\t b":   "a b",
		"  trim  ":   "trim",
		"":           "",
		"plain":      "plain",
		"行1\n行2\n行3": "行1 行2 行3",
	}
	for in, want := range cases {
		if got := oneline(in); got != want {
			t.Errorf("oneline(%q) = %q, want %q", in, got, want)
		}
	}
}

func respWithTokens(info map[string]any) *llms.ContentResponse {
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{GenerationInfo: info}}}
}

func TestUsageTokens(t *testing.T) {
	if pt, ct := usageTokens(nil); pt != 0 || ct != 0 {
		t.Error("nil resp 应返回 0/0")
	}
	if pt, ct := usageTokens(&llms.ContentResponse{}); pt != 0 || ct != 0 {
		t.Error("空 Choices 应返回 0/0")
	}
	// int 类型
	if pt, ct := usageTokens(respWithTokens(map[string]any{"PromptTokens": 10, "CompletionTokens": 20})); pt != 10 || ct != 20 {
		t.Errorf("int 类型提取错误: %d/%d", pt, ct)
	}
	// int64 类型
	if pt, ct := usageTokens(respWithTokens(map[string]any{"PromptTokens": int64(11), "CompletionTokens": int64(22)})); pt != 11 || ct != 22 {
		t.Errorf("int64 类型提取错误: %d/%d", pt, ct)
	}
	// float64 类型
	if pt, ct := usageTokens(respWithTokens(map[string]any{"PromptTokens": 12.0, "CompletionTokens": 24.0})); pt != 12 || ct != 24 {
		t.Errorf("float64 类型提取错误: %d/%d", pt, ct)
	}
	// snake_case 键名
	if pt, ct := usageTokens(respWithTokens(map[string]any{"prompt_tokens": 1, "completion_tokens": 2})); pt != 1 || ct != 2 {
		t.Errorf("snake_case 提取错误: %d/%d", pt, ct)
	}
	// 未知类型忽略
	if pt, ct := usageTokens(respWithTokens(map[string]any{"PromptTokens": "bad"})); pt != 0 || ct != 0 {
		t.Errorf("未知类型应忽略: %d/%d", pt, ct)
	}
}

func TestExtractReasoning(t *testing.T) {
	if got := extractReasoning(&llms.ContentResponse{}); got != "" {
		t.Errorf("空 Choices 应返回空: %q", got)
	}
	r := &llms.ContentResponse{Choices: []*llms.ContentChoice{{ReasoningContent: "思考链"}}}
	if got := extractReasoning(r); got != "思考链" {
		t.Errorf("ReasoningContent 提取错误: %q", got)
	}
	r = &llms.ContentResponse{Choices: []*llms.ContentChoice{{}}}
	if got := extractReasoning(r); got != "" {
		t.Errorf("空 reasoning 应返回空: %q", got)
	}
}

func TestLoggerNilAndDisabledSafety(t *testing.T) {
	var l *Logger
	// nil 与禁用时均不应 panic
	l.logRequest("id", "p", "m", nil)
	l.logResponse("id", "p", "m", nil, nil, 0)
	off := New(config.IoLog{})
	off.logRequest("", "p", "m", nil)
}

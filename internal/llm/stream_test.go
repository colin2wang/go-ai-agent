package llm

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestStreamAccumulatorJoinsChunks(t *testing.T) {
	acc := &StreamAccumulator{}
	for _, chunk := range []string{"你", "好", "，", "世", "界"} {
		if err := acc.Callback(context.Background(), []byte(chunk)); err != nil {
			t.Fatalf("Callback: %v", err)
		}
	}
	if got := acc.AnswerText(); got != "你好，世界" {
		t.Errorf("AnswerText = %q", got)
	}
}

func TestStreamAccumulatorSplitUTF8AcrossChunks(t *testing.T) {
	// 多字节字符被切在两个 chunk 之间时，按字节累积仍能正确还原
	acc := &StreamAccumulator{}
	full := []byte("中文边界")
	mid := len(full) / 2
	if err := acc.Callback(context.Background(), full[:mid]); err != nil {
		t.Fatal(err)
	}
	if err := acc.Callback(context.Background(), full[mid:]); err != nil {
		t.Fatal(err)
	}
	if got := acc.AnswerText(); got != "中文边界" {
		t.Errorf("跨 chunk 拼接 = %q", got)
	}
}

func TestStreamAccumulatorEmptyChunks(t *testing.T) {
	acc := &StreamAccumulator{}
	_ = acc.Callback(context.Background(), nil)
	_ = acc.Callback(context.Background(), []byte(""))
	if got := acc.AnswerText(); got != "" {
		t.Errorf("空 chunk 不应产生内容，得到 %q", got)
	}
}

func TestStreamAccumulatorConcurrentSafe(t *testing.T) {
	acc := &StreamAccumulator{}
	const goroutines, perGoroutine = 32, 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				_ = acc.Callback(context.Background(), []byte("x"))
				acc.AppendReasoning("y")
			}
		}()
	}
	wg.Wait()
	want := goroutines * perGoroutine
	if got := len(acc.AnswerText()); got != want {
		t.Errorf("并发累积长度 = %d，期望 %d", got, want)
	}
	if got := len(acc.ReasoningText()); got != want {
		t.Errorf("并发推理累积长度 = %d，期望 %d", got, want)
	}
}

func TestStreamAccumulatorReasoningSeparateLane(t *testing.T) {
	// 推理与回答分道：Callback 只进 Answer，推理需显式 AppendReasoning
	acc := &StreamAccumulator{}
	_ = acc.Callback(context.Background(), []byte("answer"))
	if got := acc.ReasoningText(); got != "" {
		t.Errorf("未显式写入时推理应为空，得到 %q", got)
	}
	acc.AppendReasoning("think")
	if got := acc.AnswerText(); got != "answer" {
		t.Errorf("写入推理不应影响回答，得到 %q", got)
	}
	if got := acc.ReasoningText(); got != "think" {
		t.Errorf("ReasoningText = %q", got)
	}
}

func TestStreamAccumulatorReset(t *testing.T) {
	acc := &StreamAccumulator{}
	_ = acc.Callback(context.Background(), []byte("a"))
	acc.AppendReasoning("b")
	acc.Reset()
	if acc.AnswerText() != "" || acc.ReasoningText() != "" {
		t.Errorf("Reset 后应为空，得到 %q / %q", acc.AnswerText(), acc.ReasoningText())
	}
}

func TestStreamOptionsShape(t *testing.T) {
	acc := &StreamAccumulator{}
	// 非流式：只有温度与 max-tokens 两个选项
	if n := len(StreamOptions(false, acc, 0.2, 100)); n != 2 {
		t.Errorf("非流式选项数 = %d，期望 2", n)
	}
	// 流式且带累积器：追加一个 streaming func
	if n := len(StreamOptions(true, acc, 0.2, 100)); n != 3 {
		t.Errorf("流式选项数 = %d，期望 3", n)
	}
	// 流式但无累积器：不加 streaming func（避免 langchaingo 回调 nil 累积器）
	if n := len(StreamOptions(true, nil, 0.2, 100)); n != 2 {
		t.Errorf("无累积器时选项数 = %d，期望 2", n)
	}
}

// 保证累积器在真实流式写入路径（逐 chunk）下的完整性与顺序
func TestStreamAccumulatorPreservesOrder(t *testing.T) {
	acc := &StreamAccumulator{}
	var want strings.Builder
	for i := 0; i < 200; i++ {
		chunk := strings.Repeat("ab", i%7+1)
		want.WriteString(chunk)
		if err := acc.Callback(context.Background(), []byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := acc.AnswerText(); got != want.String() {
		t.Errorf("累积结果顺序/内容不一致（长度 %d vs %d）", len(got), want.Len())
	}
}

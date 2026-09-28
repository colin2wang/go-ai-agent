package llm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

// memStore 内存版 CacheStore，便于单测。
type memStore struct {
	get  map[string]cacheRow
	fail bool
}

type cacheRow struct {
	provider, model, answer, reasoning string
	promptTokens, completionTokens     int
	expiresAt                          time.Time
}

func newMemStore() *memStore { return &memStore{get: map[string]cacheRow{}} }

func (m *memStore) CacheGet(key string) (provider, model, answer, reasoning string, promptTokens, completionTokens int, ok bool) {
	if m.fail {
		return "", "", "", "", 0, 0, false
	}
	r, ok := m.get[key]
	if !ok {
		return "", "", "", "", 0, 0, false
	}
	if !r.expiresAt.IsZero() && time.Now().After(r.expiresAt) {
		return "", "", "", "", 0, 0, false
	}
	return r.provider, r.model, r.answer, r.reasoning, r.promptTokens, r.completionTokens, true
}

func (m *memStore) CachePut(key, provider, model, answer, reasoning string, promptTokens, completionTokens int, expiresAt time.Time) error {
	if m.fail {
		return errStoreFailed
	}
	m.get[key] = cacheRow{provider, model, answer, reasoning, promptTokens, completionTokens, expiresAt}
	return nil
}

var errStoreFailed = context.DeadlineExceeded

func sampleMessages() []llms.MessageContent {
	return []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, "sys"),
		llms.TextParts(llms.ChatMessageTypeHuman, "page-1"),
	}
}

func TestCacheKeyDeterministicAndSensitive(t *testing.T) {
	p := cacheParams{Temperature: 0.2, MaxTokens: 100}
	k1 := CacheKey("prov", "model", sampleMessages(), p)
	k2 := CacheKey("prov", "model", sampleMessages(), p)
	if k1 == "" || k1 != k2 {
		t.Fatalf("相同输入应得到稳定 key：%q vs %q", k1, k2)
	}

	cases := []struct {
		name     string
		provider string
		model    string
		messages []llms.MessageContent
		params   cacheParams
	}{
		{"换 provider", "other", "model", sampleMessages(), p},
		{"换 model", "prov", "other", sampleMessages(), p},
		{"换消息", "prov", "model", []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "page-2")}, p},
		{"换温度", "prov", "model", sampleMessages(), cacheParams{Temperature: 0.9, MaxTokens: 100}},
		{"换 max-tokens", "prov", "model", sampleMessages(), cacheParams{Temperature: 0.2, MaxTokens: 200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if CacheKey(tc.provider, tc.model, tc.messages, tc.params) == k1 {
				t.Errorf("输入不同应得到不同 key")
			}
		})
	}
}

// ---- 多模态图片的键处理 ----

func imageMessages(url string) []llms.MessageContent {
	return []llms.MessageContent{
		{
			Role: llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{
				llms.TextContent{Text: "ocr"},
				llms.ImageURLContent{URL: url},
			},
		},
	}
}

func TestCacheKeyImageDigest(t *testing.T) {
	big := "data:image/png;base64," + strings.Repeat("A", 200_000)
	bigOther := "data:image/png;base64," + strings.Repeat("A", 199_999) + "B"
	small := "data:image/png;base64,AAAA"
	p := cacheParams{Temperature: 0.2}

	kBig := CacheKey("p", "m", imageMessages(big), p)
	if kBig == "" {
		t.Fatal("图片消息也应产出缓存键")
	}
	// 同图同参 → 键稳定
	if kBig != CacheKey("p", "m", imageMessages(big), p) {
		t.Error("相同图片应得到稳定 key")
	}
	// 图片内容不同 → 键不同（摘要必须能区分）
	if kBig == CacheKey("p", "m", imageMessages(bigOther), p) {
		t.Error("不同图片不应命中同一 key")
	}
	// 键长与图片体积无关（避免超长键拖慢哈希与存储）
	kSmall := CacheKey("p", "m", imageMessages(small), p)
	if len(kBig) != len(kSmall) || len(kBig) > 128 {
		t.Errorf("键长应与图片体积无关：big=%d small=%d", len(kBig), len(kSmall))
	}
	// mime 不同但正文相同 → 键不同（类型也是输入的一部分）
	jpeg := "data:image/jpeg;base64," + strings.Repeat("A", 200_000)
	if kBig == CacheKey("p", "m", imageMessages(jpeg), p) {
		t.Error("不同 mime 应得到不同 key")
	}
}

func TestDigestDataURL(t *testing.T) {
	// 非 data URL 原样返回
	const remote = "https://example.com/a.png"
	if got := digestDataURL(remote); got != remote {
		t.Errorf("外链不应改写，得到 %q", got)
	}
	// data URL 替换为摘要，且保留 mime 前缀
	got := digestDataURL("data:image/png;base64,AAAA")
	if !strings.HasPrefix(got, "data:image/png;base64,sha256-") {
		t.Errorf("应保留 mime 并换成摘要，得到 %q", got)
	}
	// 相同内容 → 相同摘要；不同内容 → 不同摘要
	a := digestDataURL("data:image/png;base64,AAAA")
	b := digestDataURL("data:image/png;base64,AAAB")
	if a == b {
		t.Error("不同内容摘要不应相同")
	}
	// 没有 base64 标记的原样返回
	if got := digestDataURL("data:image/png,AAAA"); got != "data:image/png,AAAA" {
		t.Errorf("无 base64 标记的 data URL 不应改写，得到 %q", got)
	}
}

func TestNormalizeForHashKeepsOtherParts(t *testing.T) {
	msgs := imageMessages("data:image/png;base64,AAAA")
	out := normalizeForHash(msgs)
	if len(out) != 1 || len(out[0].Parts) != 2 {
		t.Fatalf("结构应保持，得到 %+v", out)
	}
	if out[0].Role != llms.ChatMessageTypeHuman {
		t.Errorf("role 应保留，得到 %v", out[0].Role)
	}
	if tc, ok := out[0].Parts[0].(llms.TextContent); !ok || tc.Text != "ocr" {
		t.Errorf("文本部分不应被改写，得到 %#v", out[0].Parts[0])
	}
	// 原消息未被就地修改（深拷贝）：调用方的 URL 仍应是未摘要的原文
	const original = "data:image/png;base64,AAAA"
	if img, ok := msgs[0].Parts[1].(llms.ImageURLContent); !ok || img.URL != original {
		t.Errorf("应深拷贝，不修改调用方传入的消息，得到 %q", msgs[0].Parts[1])
	}
}

func TestCacheEnabledNilSafety(t *testing.T) {
	var c *Cache
	if c.Enabled() {
		t.Error("nil Cache 应恒不命中")
	}
	if c.TTL() != 0 {
		t.Error("nil Cache TTL 应为 0")
	}
	if NewCache(nil, CacheConfig{Enabled: true}).Enabled() {
		t.Error("无 store 时不应启用")
	}
	if NewCache(newMemStore(), CacheConfig{Enabled: false}).Enabled() {
		t.Error("Enabled=false 不应启用")
	}
	if !NewCache(newMemStore(), CacheConfig{Enabled: true}).Enabled() {
		t.Error("Enabled=true 且有 store 时应启用")
	}
}

func TestCacheTTL(t *testing.T) {
	c := NewCache(newMemStore(), CacheConfig{Enabled: true, TTL: 2 * time.Hour})
	if c.TTL() != 2*time.Hour {
		t.Errorf("TTL = %v", c.TTL())
	}
	// 负值归一为 0（不过期）
	c2 := NewCache(newMemStore(), CacheConfig{Enabled: true, TTL: -1})
	if c2.TTL() != 0 {
		t.Errorf("负 TTL 应归一为 0，得到 %v", c2.TTL())
	}
}

func TestCacheDecisionContextSwitch(t *testing.T) {
	c := NewCache(newMemStore(), CacheConfig{Enabled: true})
	if !c.decision(context.Background(), true) {
		t.Error("全局开启且未标记时应走缓存")
	}
	if c.decision(context.Background(), false) {
		t.Error("全局默认关闭时应不走缓存")
	}
	ctx := WithCacheEnabled(context.Background(), false)
	if c.decision(ctx, true) {
		t.Error("调用级显式 false 应绕过缓存")
	}
	ctxOn := WithCacheEnabled(context.Background(), true)
	if !c.decision(ctxOn, false) {
		t.Error("调用级显式 true 应启用缓存")
	}
	off := NewCache(newMemStore(), CacheConfig{Enabled: false})
	if off.decision(WithCacheEnabled(context.Background(), true), true) {
		t.Error("全局关闭时调用级开关不应生效")
	}
}

func TestCacheGetPutRoundTripAndExpiry(t *testing.T) {
	store := newMemStore()

	key := CacheKey("prov", "model", sampleMessages(), cacheParams{Temperature: 0.1})
	if _, _, _, _, _, _, ok := store.CacheGet(key); ok {
		t.Fatal("写入前不应命中")
	}
	if err := store.CachePut(key, "prov", "model", "answer-1", "think-1", 11, 22, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CachePut: %v", err)
	}
	provider, model, answer, reasoning, pt, ct, ok := store.CacheGet(key)
	if !ok || provider != "prov" || model != "model" || answer != "answer-1" || reasoning != "think-1" || pt != 11 || ct != 22 {
		t.Errorf("round-trip 不一致: %q %q %q %q %d %d %v", provider, model, answer, reasoning, pt, ct, ok)
	}

	// 过期后不应命中
	if err := store.CachePut(key, "prov", "model", "x", "", 0, 0, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, ok := store.CacheGet(key); ok {
		t.Error("已过期条目不应命中")
	}
}

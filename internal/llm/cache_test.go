package llm

import (
	"context"
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

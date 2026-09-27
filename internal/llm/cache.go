// cache.go LLM 结果缓存：相同输入（provider+model+消息+生成参数）命中即复用，
// 避免重复烧 token。存储由外部注入（本项目为 stats 适配器），本包不依赖数据库实现。
package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/tmc/langchaingo/llms"
)

// CacheStore 缓存仓储接口（由 stats.CacheStoreAdapter 实现，避免 llm ↔ stats 循环依赖）。
type CacheStore interface {
	CacheGet(key string) (provider, model, answer, reasoning string, promptTokens, completionTokens int, ok bool)
	CachePut(key, provider, model, answer, reasoning string, promptTokens, completionTokens int, expiresAt time.Time) error
}

// CacheConfig 缓存策略参数。
type CacheConfig struct {
	Enabled   bool
	TTL       time.Duration // <=0 表示写入时不带过期（永久复用，直到被容量淘汰）
	KeepOnErr bool          // 空回答是否也落缓存（默认 false：下次仍真实重试）
}

// Cache 带 TTL 的 LLM 结果缓存包装器。
type Cache struct {
	store CacheStore
	cfg   CacheConfig
}

// NewCache 构造缓存包装器；store 为 nil 或 enabled=false 时得到恒不命中的实例。
func NewCache(store CacheStore, cfg CacheConfig) *Cache {
	return &Cache{store: store, cfg: cfg}
}

// Enabled 缓存是否可用（未启用时所有操作均为 no-op）。
func (c *Cache) Enabled() bool {
	return c != nil && c.cfg.Enabled && c.store != nil
}

// TTL 写入缓存时的有效期（0 = 不过期）。
func (c *Cache) TTL() time.Duration {
	if c == nil || c.cfg.TTL < 0 {
		return 0
	}
	return c.cfg.TTL
}

// cacheParams 参与哈希的生成参数：任一参数不同都不应命中同一份结果。
type cacheParams struct {
	Temperature float64
	MaxTokens   int
	StopWords   []string
	JSONMode    bool
	TopK        int
	TopP        float64
	Seed        int
	N           int
}

// CacheKey 计算缓存键：sha256(provider, model, messagesJSON, paramsJSON)。
// messages 为已构造的 langchaingo 结构（多模态图片以 data URL 形式内联，
// 同一张图经服务端统一编码后字符串稳定，可安全参与哈希）。
func CacheKey(provider, model string, messages []llms.MessageContent, p cacheParams) string {
	mj, err := json.Marshal(messages)
	if err != nil {
		return ""
	}
	pj, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(provider), []byte(model), mj, pj} {
		h.Write(part)
		h.Write([]byte{0}) // 长度分隔，避免拼接歧义造成碰撞
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ctxKeyCache 调用级缓存开关的上下文键（由编排层按步骤配置注入）。
type ctxKeyCache struct{}

// WithCacheEnabled 在上下文中标记本次调用是否允许读写结果缓存。
// 未标记时按全局开关执行；显式传 false 可让个别步骤（如随机性强的分析）绕过缓存。
func WithCacheEnabled(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, ctxKeyCache{}, enabled)
}

// cacheEnabledFromContext 读取调用级缓存开关（未设置时返回 def）。
func cacheEnabledFromContext(ctx context.Context, def bool) bool {
	if v, ok := ctx.Value(ctxKeyCache{}).(bool); ok {
		return v
	}
	return def
}

// decision 综合全局开关与调用级开关判断本次是否走缓存。
func (c *Cache) decision(ctx context.Context, def bool) bool {
	if !c.Enabled() {
		return false
	}
	return cacheEnabledFromContext(ctx, def)
}

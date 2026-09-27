// Package config 加载三层配置：
//
//	config.yml（总配置）
//	  ├── ai.config         → ai.yml             AI provider 端点与全局默认
//	  └── features[].config → features/*.yml     各子功能（工作流 + 步骤 + 提示词）
//
// 新增子功能只需在 config.yml 的 features 中登记一项并添加对应 yml，
// 平台代码无需改动。路径优先按当前工作目录解析，失败回退到 config.yml 所在目录。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 总配置（config.yml）+ Load 展开后的派生配置（LLM / Resolved）。
type Config struct {
	App      App          `yaml:"app"`
	Server   Server       `yaml:"server"`
	Storage  Storage      `yaml:"storage"`
	Logging  Logging      `yaml:"logging"`
	Cache    Cache        `yaml:"cache"`
	AI       AIRef        `yaml:"ai"`
	Features []FeatureRef `yaml:"features"`

	// ===== 以下为 Load 展开结果，不参与 config.yml 反序列化 =====
	LLM       LLM       `yaml:"-"`
	Resolved  []Feature `yaml:"-"`
	SourceDir string    `yaml:"-"`
}

// App 应用元信息。
type App struct {
	Name  string `yaml:"name"`
	Title string `yaml:"title"`
}

// Server HTTP 服务配置。
type Server struct {
	Addr                     string   `yaml:"addr"`
	OpenBrowser              bool     `yaml:"open-browser"`
	ReadHeaderTimeoutSeconds int      `yaml:"read-header-timeout-seconds"`
	ShutdownTimeoutSeconds   int      `yaml:"shutdown-timeout-seconds"`
	APITokens                []string `yaml:"api-tokens"` // 对外调用令牌（为空 = 不鉴权，本机单用户体验不变）
	AuthExcludeHealth        bool     `yaml:"auth-exclude-health"`
}

// Storage LLM 调用统计落库配置。
type Storage struct {
	Enabled        bool   `yaml:"enabled"`
	DBPath         string `yaml:"db-path"`
	RetentionDays  int    `yaml:"retention-days"`
	QueueCapacity  int    `yaml:"queue-capacity"`
	CapsTTLHours   int    `yaml:"caps-ttl-hours"`         // 模型能力探测缓存有效期（小时，0 = 默认 24）
	PersistJobs    bool   `yaml:"persist-jobs"`           // 异步任务落库（重启后仍可查询结果）
	PruneIntervalM int    `yaml:"prune-interval-minutes"` // 过期数据清理周期（分钟，0 = 默认 60）
}

// Cache LLM 结果缓存（按 provider+model+消息+参数 哈希复用，确定性提取任务收益最大）。
type Cache struct {
	Enabled   bool `yaml:"enabled"`
	TTLHours  int  `yaml:"ttl-hours"` // 0 = 默认 24
	MaxRows   int  `yaml:"max-rows"`  // 缓存行数上限，超出按写入时间淘汰（0 = 默认 2000）
	KeepOnErr bool `yaml:"keep-on-error"`
}

// Logging 日志配置。
type Logging struct {
	Level string `yaml:"level"`
}

// AIRef 指向 AI provider 配置文件（ai.yml）。
type AIRef struct {
	Config string `yaml:"config"`
}

// FeatureRef 子功能注册项。
type FeatureRef struct {
	ID      string `yaml:"id"`
	Enabled *bool  `yaml:"enabled"` // 省略 = 启用
	Config  string `yaml:"config"`
}

// IsEnabled 未显式声明 enabled 时默认启用。
func (f FeatureRef) IsEnabled() bool { return f.Enabled == nil || *f.Enabled }

// Feature 子功能完整配置（features/*.yml）。
type Feature struct {
	ID          string          `yaml:"id"`
	Name        string          `yaml:"name"`
	Description string          `yaml:"description"`
	Icon        string          `yaml:"icon"`
	Order       int             `yaml:"order"`
	Kind        string          `yaml:"kind"` // 页面类型：workflow（默认，上传执行）| chat（对话）
	Input       Input           `yaml:"input"`
	Output      Output          `yaml:"output"`
	Workflow    Workflow        `yaml:"workflow"`
	Steps       map[string]Step `yaml:"steps"`
	Chat        Chat            `yaml:"chat"`
}

// IsChat 对话类子功能（前端渲染聊天界面而非上传执行页）。
func (f Feature) IsChat() bool { return f.Kind == "chat" }

// Input 子功能的输入声明（前端据此渲染上传控件，服务端据此强校验）。
type Input struct {
	Kind       string `yaml:"kind"`         // image | file | text
	Multiple   bool   `yaml:"multiple"`     // 是否支持多文件（如多页扫描件）
	Accept     string `yaml:"accept"`       // input[accept]
	Hint       string `yaml:"hint"`         // 上传区提示文案
	MaxFiles   int    `yaml:"max-files"`    // 单次上传文件数上限（0 = 默认 20）
	MaxFileMB  int    `yaml:"max-file-mb"`  // 单个文件体积上限 MB（0 = 默认 16）
	MaxTotalMB int    `yaml:"max-total-mb"` // 本次上传总体积上限 MB（0 = 默认 64）
}

// Chat 对话类子功能的专属配置。
type Chat struct {
	DefaultProvider string `yaml:"default-provider"` // 缺省 provider（留空回退 ai.yml default-provider）
	SystemPrompt    string `yaml:"system-prompt"`    // 系统提示词
	StreamDefault   *bool  `yaml:"stream-default"`   // 默认是否流式（nil = 跟随全局 stream-output）
	MaxHistory      int    `yaml:"max-history"`      // 发送给模型的历史消息条数上限（0 = 不限制）
	ReasoningEffort string `yaml:"reasoning-effort"` // 思考力度：none/minimal/low/medium/high/xhigh（空 = 不发送，跟随端点默认）
	MaxImages       int    `yaml:"max-images"`       // 单条消息最多附带的图片数（0 = 默认 4，仅视觉模型可用）
	MaxImageMB      int    `yaml:"max-image-mb"`     // 单张图片解码后体积上限 MB（0 = 默认 8）
}

// validReasoningEffort chat.reasoning-effort 的合法取值（OpenAI 兼容协议，LM Studio 同样接受）。
var validReasoningEffort = map[string]bool{
	"none": true, "minimal": true, "low": true,
	"medium": true, "high": true, "xhigh": true,
}

// Output 子功能的输出声明（模板渲染，用法类似 FreeMarker）。
type Output struct {
	Template    string `yaml:"template"`     // 模板名 → templates/<category>/<name>.tpl
	TemplateDir string `yaml:"template-dir"` // 外部模板目录（留空使用内置模板）
	Extension   string `yaml:"extension"`    // 下载文件扩展名
}

// Workflow 子功能的工作流声明。
type Workflow struct {
	Type           string   `yaml:"type"`                  // 编排类型（对应 internal/graph 内置图）
	Entry          string   `yaml:"entry"`                 // 入口节点
	MaxRetries     int      `yaml:"max-retries"`           // 条件边回退重试上限
	RetryMax       int      `yaml:"retry-max"`             // 单次调用内重试上限（限流/5xx 才重试，0 = 默认 2）
	RetryBackoffMS int      `yaml:"retry-backoff-ms"`      // 重试退避基数（毫秒，0 = 不退避）
	MaxParallel    int      `yaml:"max-parallel"`          // 逐页并发度（0/1 = 串行）
	ProviderMaxReq int      `yaml:"provider-max-requests"` // 每个 provider 的并发上限（0 = 默认 2）
	Pipeline       []string `yaml:"pipeline"`              // 展示用节点顺序
}

// aiDoc 对应 ai.yml 的根（`llm:` 节点）。
type aiDoc struct {
	LLM LLM `yaml:"llm"`
}

// LLM AI provider 端点与全局默认（ai.yml 的 llm 节点）。
type LLM struct {
	// 全局默认回退值
	BaseURL               string  `yaml:"base-url"`
	APIKey                string  `yaml:"api-key"`
	Temperature           float64 `yaml:"temperature"`
	TimeoutSeconds        int     `yaml:"timeout-seconds"`
	ConnectTimeoutSeconds int     `yaml:"connect-timeout-seconds"`
	MaxTokens             int     `yaml:"max-tokens"`
	DebugRequestLog       bool    `yaml:"debug-request-log"`

	IoLog            IoLog      `yaml:"io-log"`
	StreamOutput     bool       `yaml:"stream-output"`
	ReasoningEnabled bool       `yaml:"reasoning-enabled"`
	DefaultProvider  string     `yaml:"default-provider"`
	Providers        []Provider `yaml:"providers"`
}

// IoLog 输入/输出截断日志配置。
type IoLog struct {
	Enabled   bool `yaml:"enabled"`
	HeadChars int  `yaml:"head-chars"`
	TailChars int  `yaml:"tail-chars"`
	Debug     bool `yaml:"debug"` // 调试模式：完整输出输入文本与模型原始输出（不截断、不折叠）
}

// Provider 一个连接端点声明。
type Provider struct {
	Name                  string `yaml:"name"`
	Type                  string `yaml:"type"`
	BaseURL               string `yaml:"base-url"`
	APIKey                string `yaml:"api-key"`
	TimeoutSeconds        int    `yaml:"timeout-seconds"`
	ConnectTimeoutSeconds int    `yaml:"connect-timeout-seconds"`
	MaxTokens             int    `yaml:"max-tokens"`
}

// Step 单个工作流步骤的配置：引用 provider + 模型 + 提示词 + 覆盖参数。
type Step struct {
	Provider         string   `yaml:"provider"`
	Model            string   `yaml:"model"`
	PromptTemplate   string   `yaml:"prompt-template"` // 预留：内置模板名
	SystemPrompt     string   `yaml:"system-prompt"`   // 系统提示词（直接写在 yml，优先于内置模板）
	UserPrompt       string   `yaml:"user-prompt"`     // 用户提示词（直接写在 yml，缺省回退内置默认）
	Temperature      *float64 `yaml:"temperature"`     // 指针：区分「未设置」与 0
	MaxTokens        int      `yaml:"max-tokens"`
	ReasoningEnabled *bool    `yaml:"reasoning-enabled"` // nil = 回退全局
	Cache            *bool    `yaml:"cache"`             // nil = 跟随全局 cache.enabled（结果缓存开关）
}

// WithDefaults 用 def 补齐未声明的字段，返回可直接使用的步骤配置。
func (s Step) WithDefaults(def Step) Step {
	out := s
	if out.Provider == "" {
		out.Provider = def.Provider
	}
	if out.Model == "" {
		out.Model = def.Model
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = def.MaxTokens
	}
	if out.PromptTemplate == "" {
		out.PromptTemplate = def.PromptTemplate
	}
	if out.Temperature == nil {
		out.Temperature = def.Temperature
	}
	if out.ReasoningEnabled == nil {
		out.ReasoningEnabled = def.ReasoningEnabled
	}
	if out.SystemPrompt == "" {
		out.SystemPrompt = def.SystemPrompt
	}
	if out.UserPrompt == "" {
		out.UserPrompt = def.UserPrompt
	}
	return out
}

// Temp 返回温度值（未设置时为 0）。
func (s Step) Temp() float64 {
	if s.Temperature == nil {
		return 0
	}
	return *s.Temperature
}

// Load 读取总配置，并展开 ai.yml 与各子功能配置文件。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.SourceDir = filepath.Dir(path)

	// 1) AI provider：ai.yml
	aiPath := c.resolve(c.AI.Config, "ai.yml")
	var ai aiDoc
	if err := loadYAML(aiPath, &ai); err != nil {
		return nil, err
	}
	c.LLM = ai.LLM
	c.expandLLM()

	// 2) 子功能：features/*.yml
	for _, ref := range c.Features {
		if !ref.IsEnabled() {
			continue
		}
		if ref.ID == "" {
			return nil, fmt.Errorf("config.features: 存在缺少 id 的注册项")
		}
		fp := c.resolve(ref.Config, filepath.Join("features", ref.ID+".yml"))
		var f Feature
		if err := loadYAML(fp, &f); err != nil {
			return nil, err
		}
		if f.ID == "" {
			f.ID = ref.ID
		}
		if f.Name == "" {
			f.Name = f.ID
		}
		c.Resolved = append(c.Resolved, f)
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// loadYAML 读取并解析单个 yml 到目标结构体。
func loadYAML(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

// resolve 解析引用路径：绝对路径直接用；其次按当前工作目录；最后按 config.yml 所在目录。
// 因此配置里建议写「相对 config.yml 自身」的路径（如 ai.yml、features/payslip-ocr.yml），
// 这样无论从哪个工作目录启动、-config 指向何处都能正确解析。
func (c *Config) resolve(ref, def string) string {
	if ref == "" {
		ref = def
	}
	if ref == "" || filepath.IsAbs(ref) {
		return ref
	}
	if _, err := os.Stat(ref); err == nil {
		return ref
	}
	alt := filepath.Join(c.SourceDir, ref)
	if _, err := os.Stat(alt); err == nil {
		return alt
	}
	return ref
}

// expandLLM 为 provider 补齐全局回退值，展开 ${ENV:default} 形式的 api-key。
func (c *Config) expandLLM() {
	g := &c.LLM
	for i := range g.Providers {
		p := &g.Providers[i]
		if p.BaseURL == "" {
			p.BaseURL = g.BaseURL
		}
		if p.APIKey == "" {
			p.APIKey = g.APIKey
		}
		if p.TimeoutSeconds == 0 {
			p.TimeoutSeconds = g.TimeoutSeconds
		}
		if p.ConnectTimeoutSeconds == 0 {
			p.ConnectTimeoutSeconds = g.ConnectTimeoutSeconds
		}
		if p.MaxTokens == 0 {
			p.MaxTokens = g.MaxTokens
		}
		p.APIKey = expandEnv(p.APIKey)
	}
}

// expandEnv 支持 ${VAR} 与 ${VAR:default} 语法。
func expandEnv(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return os.Expand(s, func(key string) string {
		// os.Expand 传入的 key 是 ${} 内部原文，如 "OPENAI_API_KEY:sk-your-key-here"
		if name, def, ok := strings.Cut(key, ":"); ok {
			if v := os.Getenv(name); v != "" {
				return v
			}
			return def
		}
		return os.Getenv(key)
	})
}

// Feature 按 id 查找已启用的子功能。
func (c *Config) Feature(id string) (Feature, bool) {
	for _, f := range c.Resolved {
		if f.ID == id {
			return f, true
		}
	}
	return Feature{}, false
}

// ---- 上传/并发/缓存的服务端默认上限（features yml 可逐项覆盖）----
const (
	defaultInputMaxFiles    = 20
	defaultInputMaxFileMB   = 16
	defaultInputMaxTotalMB  = 64
	defaultWorkflowParallel = 1
	defaultProviderMaxReq   = 2
	defaultCacheTTLHours    = 24
	defaultCacheMaxRows     = 2000
	defaultCapsTTLHours     = 24
	defaultPruneIntervalMin = 60
)

// FileCountLimit / FileBytesLimit / TotalBytesLimit 上传限制（未声明时取默认值）。
func (in Input) FileCountLimit() int {
	if in.MaxFiles > 0 {
		return in.MaxFiles
	}
	return defaultInputMaxFiles
}

func (in Input) FileBytesLimit() int64 {
	mb := in.MaxFileMB
	if mb <= 0 {
		mb = defaultInputMaxFileMB
	}
	return int64(mb) << 20
}

func (in Input) TotalBytesLimit() int64 {
	mb := in.MaxTotalMB
	if mb <= 0 {
		mb = defaultInputMaxTotalMB
	}
	return int64(mb) << 20
}

// Parallel 逐页并发度（<=0 归一为 1，即保持原有串行行为）。
func (w Workflow) Parallel() int {
	if w.MaxParallel <= 0 {
		return defaultWorkflowParallel
	}
	return w.MaxParallel
}

// CallRetryMax 单次调用内的重试上限：未声明时，配置了退避则默认重试 2 次，
// 否则保持旧行为（不重试）。
func (w Workflow) CallRetryMax() int {
	if w.RetryMax > 0 {
		return w.RetryMax
	}
	if w.RetryBackoffMS > 0 {
		return 2
	}
	return 0
}

// ProviderConcurrency 单个 provider 允许的并发请求数。
func (w Workflow) ProviderConcurrency() int {
	if w.ProviderMaxReq <= 0 {
		return defaultProviderMaxReq
	}
	return w.ProviderMaxReq
}

// CacheEnabled 该步骤是否参与结果缓存（步骤显式声明优先于全局开关）。
func (s Step) CacheEnabled(global bool) bool {
	if s.Cache != nil {
		return *s.Cache
	}
	return global
}

// CacheTTLHours / CacheMaxRows 结果缓存的过期与容量（未声明时取默认值）。
// 方法名不能与字段同名，故加 Cache 前缀。
func (c Cache) CacheTTLHours() int {
	if c.TTLHours > 0 {
		return c.TTLHours
	}
	return defaultCacheTTLHours
}

func (c Cache) CacheMaxRows() int {
	if c.MaxRows > 0 {
		return c.MaxRows
	}
	return defaultCacheMaxRows
}

// TTL 缓存有效期的时长形式（供调用处直接使用）。
func (c Cache) TTL() time.Duration { return time.Duration(c.CacheTTLHours()) * time.Hour }

func (st Storage) CapsTTL() int {
	if st.CapsTTLHours > 0 {
		return st.CapsTTLHours
	}
	return defaultCapsTTLHours
}

func (st Storage) PruneInterval() time.Duration {
	m := st.PruneIntervalM
	if m <= 0 {
		m = defaultPruneIntervalMin
	}
	return time.Duration(m) * time.Minute
}

// validate 启动期校验：provider 引用、步骤模型、存储路径。
func (c *Config) validate() error {
	g := &c.LLM
	names := make(map[string]bool, len(g.Providers))
	for _, p := range g.Providers {
		if p.Name == "" {
			return fmt.Errorf("ai.providers: provider 缺少 name")
		}
		if names[p.Name] {
			return fmt.Errorf("ai.providers: provider %q 重复声明", p.Name)
		}
		names[p.Name] = true
	}
	if len(names) == 0 {
		return fmt.Errorf("ai.providers: 至少需要声明一个 provider")
	}
	if g.DefaultProvider != "" && !names[g.DefaultProvider] {
		return fmt.Errorf("ai.default-provider %q 不在 providers 列表中", g.DefaultProvider)
	}

	seen := map[string]bool{}
	for _, f := range c.Resolved {
		if seen[f.ID] {
			return fmt.Errorf("config.features: 子功能 %q 重复注册", f.ID)
		}
		seen[f.ID] = true
		// 对话类子功能也必须声明一个 chat 步骤（承载默认 provider/model/参数）
		if len(f.Steps) == 0 {
			return fmt.Errorf("features.%s: 未配置任何 steps", f.ID)
		}
		if f.IsChat() {
			if f.Chat.DefaultProvider != "" && !names[f.Chat.DefaultProvider] {
				return fmt.Errorf("features.%s.chat.default-provider %q 未声明", f.ID, f.Chat.DefaultProvider)
			}
			if f.Chat.ReasoningEffort != "" && !validReasoningEffort[f.Chat.ReasoningEffort] {
				return fmt.Errorf("features.%s.chat.reasoning-effort %q 无效（支持 none/minimal/low/medium/high/xhigh，留空则不发送）", f.ID, f.Chat.ReasoningEffort)
			}
			if f.Chat.MaxImages < 0 {
				return fmt.Errorf("features.%s.chat.max-images 不能为负数", f.ID)
			}
			if f.Chat.MaxImageMB < 0 {
				return fmt.Errorf("features.%s.chat.max-image-mb 不能为负数", f.ID)
			}
		}
		// 上传与并发参数的合法性（非负）。
		if f.Input.MaxFiles < 0 || f.Input.MaxFileMB < 0 || f.Input.MaxTotalMB < 0 {
			return fmt.Errorf("features.%s.input 的 max-files/max-file-mb/max-total-mb 不能为负数", f.ID)
		}
		if f.Workflow.MaxParallel < 0 {
			return fmt.Errorf("features.%s.workflow.max-parallel 不能为负数", f.ID)
		}
		if f.Workflow.ProviderMaxReq < 0 {
			return fmt.Errorf("features.%s.workflow.provider-max-requests 不能为负数", f.ID)
		}
		if f.Workflow.RetryBackoffMS < 0 || f.Workflow.RetryMax < 0 {
			return fmt.Errorf("features.%s.workflow.retry-backoff-ms/retry-max 不能为负数", f.ID)
		}
		for name, st := range f.Steps {
			// 提示词（系统/用户）必须写在对应 features/*.yml 的 system-prompt/user-prompt 中，
			// 代码内不再硬编码任何提示词。对话类走 chat.system-prompt，render 为本地模板渲染。
			if !f.IsChat() && name != "render" && strings.TrimSpace(st.SystemPrompt) == "" {
				return fmt.Errorf("features.%s.steps.%s: 缺少 system-prompt（提示词必须写在对应 features/*.yml 中）", f.ID, name)
			}
			if st.Model == "" {
				return fmt.Errorf("features.%s.steps.%s: 模型无全局默认，必须显式指定 model", f.ID, name)
			}
			p := st.Provider
			if p == "" {
				p = g.DefaultProvider
			}
			if p != "" && !names[p] {
				return fmt.Errorf("features.%s.steps.%s: 引用的 provider %q 未声明", f.ID, name, p)
			}
		}
	}

	if c.Storage.Enabled && c.Storage.DBPath == "" {
		return fmt.Errorf("storage.enabled=true 但未配置 db-path")
	}
	if c.Cache.Enabled && !c.Storage.Enabled {
		return fmt.Errorf("cache.enabled=true 需要 storage.enabled=true（结果缓存复用同一个 SQLite）")
	}
	if c.Storage.CapsTTLHours < 0 || c.Storage.PruneIntervalM < 0 {
		return fmt.Errorf("storage.caps-ttl-hours/prune-interval-minutes 不能为负数")
	}
	if c.Cache.TTLHours < 0 || c.Cache.MaxRows < 0 {
		return fmt.Errorf("cache.ttl-hours/max-rows 不能为负数")
	}
	for i, tok := range c.Server.APITokens {
		if strings.TrimSpace(tok) == "" {
			return fmt.Errorf("server.api-tokens[%d] 为空，请删除该项或填入非空令牌", i)
		}
	}
	return nil
}

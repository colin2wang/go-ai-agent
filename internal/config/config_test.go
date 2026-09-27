package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAll 在临时目录里落一组文件，返回目录路径。
func writeAll(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const baseAI = `
llm:
  base-url: http://localhost:1234/v1
  api-key: ${TEST_KEY:sk-default}
  default-provider: local
  providers:
    - name: local
      type: openai
      base-url: http://localhost:1234/v1
`

const baseFeature = `
id: demo
name: 演示
kind: workflow
workflow:
  type: book-toc
  entry: ocr
steps:
  ocr:
    provider: local
    model: demo-model
    system-prompt: 你是 OCR 助手
`

const baseConfig = `
app:
  name: test
ai:
  config: ai.yml
features:
  - id: demo
`

// loadOK 加载最小合法配置，返回 *Config。
func loadOK(t *testing.T) *Config {
	t.Helper()
	dir := writeAll(t, map[string]string{
		"config.yml":        baseConfig,
		"ai.yml":            baseAI,
		"features/demo.yml": baseFeature,
	})
	c, err := Load(filepath.Join(dir, "config.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestLoadResolvesThreeLayers(t *testing.T) {
	c := loadOK(t)

	if len(c.Resolved) != 1 {
		t.Fatalf("期望解析出 1 个子功能，得到 %d", len(c.Resolved))
	}
	f := c.Resolved[0]
	if f.ID != "demo" || f.Name != "演示" {
		t.Errorf("子功能 id/name = %q/%q", f.ID, f.Name)
	}
	if f.Kind != "workflow" || f.IsChat() {
		t.Errorf("kind = %q，IsChat 应为 false", f.Kind)
	}
	// ai.yml 的 LLM 节点已展开
	if len(c.LLM.Providers) != 1 || c.LLM.Providers[0].Name != "local" {
		t.Fatalf("providers 展开错误: %+v", c.LLM.Providers)
	}
	if c.LLM.DefaultProvider != "local" {
		t.Errorf("default-provider = %q", c.LLM.DefaultProvider)
	}
	// 步骤可按名查找
	st, ok := f.Steps["ocr"]
	if !ok || st.Model != "demo-model" || st.SystemPrompt == "" {
		t.Errorf("步骤 ocr 未正确加载: %+v", st)
	}
}

func TestLoadEnvExpansion(t *testing.T) {
	t.Setenv("TEST_KEY", "sk-from-env")
	c := loadOK(t)
	if got := c.LLM.Providers[0].APIKey; got != "sk-from-env" {
		t.Errorf("环境变量优先未生效，api-key = %q", got)
	}

	t.Setenv("TEST_KEY", "")
	c2 := loadOK(t)
	if got := c2.LLM.Providers[0].APIKey; got != "sk-default" {
		t.Errorf("${ENV:default} 回退未生效，api-key = %q", got)
	}
}

func TestLoadDisabledFeatureSkipped(t *testing.T) {
	dir := writeAll(t, map[string]string{
		"config.yml": `
ai:
  config: ai.yml
features:
  - id: demo
    enabled: false
  - id: demo2
`,
		"ai.yml":             baseAI,
		"features/demo.yml":  baseFeature,
		"features/demo2.yml": strings.Replace(baseFeature, "demo\n", "demo2\n", 1),
	})
	c, err := Load(filepath.Join(dir, "config.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Resolved) != 1 || c.Resolved[0].ID != "demo2" {
		t.Fatalf("enabled:false 应被跳过，Resolved = %+v", c.Resolved)
	}
}

func TestLoadPathFallbackToSourceDir(t *testing.T) {
	// 从其它工作目录加载，路径应回退到 config.yml 所在目录
	c := loadOK(t)
	dir := c.SourceDir
	other, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(filepath.Join(dir, "config.yml"))
	if err != nil {
		t.Fatalf("Load from other cwd: %v", err)
	}
	if len(c2.Resolved) != 1 {
		t.Fatalf("回退解析失败，Resolved = %+v", c2.Resolved)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		ai      string
		cfg     string
		feat    string
		wantSub string
	}{
		{
			name:    "provider 缺少 name",
			ai:      "llm:\n  providers:\n    - type: openai\n",
			cfg:     baseConfig,
			feat:    baseFeature,
			wantSub: "缺少 name",
		},
		{
			name:    "无 provider",
			ai:      "llm:\n  providers: []\n",
			cfg:     baseConfig,
			feat:    baseFeature,
			wantSub: "至少需要声明一个 provider",
		},
		{
			name:    "default-provider 未声明",
			ai:      "llm:\n  default-provider: ghost\n  providers:\n    - name: local\n      type: openai\n",
			cfg:     baseConfig,
			feat:    baseFeature,
			wantSub: "不在 providers 列表中",
		},
		{
			name:    "步骤缺少 model",
			ai:      baseAI,
			cfg:     baseConfig,
			feat:    "id: demo\nkind: workflow\nsteps:\n  ocr:\n    provider: local\n    system-prompt: x\n",
			wantSub: "必须显式指定 model",
		},
		{
			name:    "非 chat 步骤缺少 system-prompt",
			ai:      baseAI,
			cfg:     baseConfig,
			feat:    "id: demo\nkind: workflow\nsteps:\n  ocr:\n    provider: local\n    model: m\n",
			wantSub: "缺少 system-prompt",
		},
		{
			name:    "步骤引用未声明 provider",
			ai:      baseAI,
			cfg:     baseConfig,
			feat:    "id: demo\nkind: workflow\nsteps:\n  ocr:\n    provider: ghost\n    model: m\n    system-prompt: x\n",
			wantSub: "未声明",
		},
		{
			name:    "chat 引用未声明 provider",
			ai:      baseAI,
			cfg:     baseConfig,
			feat:    "id: demo\nkind: chat\nchat:\n  default-provider: ghost\nsteps:\n  chat:\n    provider: local\n    model: m\n",
			wantSub: "未声明",
		},
		{
			name:    "chat reasoning-effort 非法",
			ai:      baseAI,
			cfg:     baseConfig,
			feat:    "id: demo\nkind: chat\nchat:\n  reasoning-effort: ultra\nsteps:\n  chat:\n    provider: local\n    model: m\n",
			wantSub: "reasoning-effort",
		},
		{
			name:    "无 steps",
			ai:      baseAI,
			cfg:     baseConfig,
			feat:    "id: demo\nkind: workflow\n",
			wantSub: "未配置任何 steps",
		},
		{
			name:    "负数上传限制",
			ai:      baseAI,
			cfg:     baseConfig,
			feat:    "id: demo\nkind: workflow\ninput:\n  max-files: -1\nsteps:\n  ocr:\n    provider: local\n    model: m\n    system-prompt: x\n",
			wantSub: "不能为负数",
		},
		{
			name:    "cache 需要storage",
			ai:      baseAI,
			cfg:     "ai:\n  config: ai.yml\ncache:\n  enabled: true\nfeatures:\n  - id: demo\n",
			feat:    baseFeature,
			wantSub: "cache.enabled=true 需要 storage.enabled=true",
		},
		{
			name:    "storage 缺 db-path",
			ai:      baseAI,
			cfg:     "ai:\n  config: ai.yml\nstorage:\n  enabled: true\nfeatures:\n  - id: demo\n",
			feat:    baseFeature,
			wantSub: "db-path",
		},
		{
			name:    "空 api-token",
			ai:      baseAI,
			cfg:     "ai:\n  config: ai.yml\nserver:\n  api-tokens: [\"ok\", \"  \"]\nfeatures:\n  - id: demo\n",
			feat:    baseFeature,
			wantSub: "api-tokens",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeAll(t, map[string]string{
				"config.yml":        tc.cfg,
				"ai.yml":            tc.ai,
				"features/demo.yml": tc.feat,
			})
			if _, err := Load(filepath.Join(dir, "config.yml")); err == nil {
				t.Fatalf("期望报错，实际成功")
			} else if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误 %q 不含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidateDuplicateFeatureRegistration(t *testing.T) {
	dir := writeAll(t, map[string]string{
		"config.yml": `
ai:
  config: ai.yml
features:
  - id: demo
  - id: demo
`,
		"ai.yml":            baseAI,
		"features/demo.yml": baseFeature,
	})
	_, err := Load(filepath.Join(dir, "config.yml"))
	if err == nil || !strings.Contains(err.Error(), "重复注册") {
		t.Fatalf("期望重复注册报错，得到 %v", err)
	}
}

func TestLoadMissingFeatureFile(t *testing.T) {
	dir := writeAll(t, map[string]string{
		"config.yml": baseConfig,
		"ai.yml":     baseAI,
		// 缺 features/demo.yml
	})
	_, err := Load(filepath.Join(dir, "config.yml"))
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("期望读取失败报错，得到 %v", err)
	}
}

func TestLimitsAndDefaults(t *testing.T) {
	var in Input
	if got := in.FileCountLimit(); got != defaultInputMaxFiles {
		t.Errorf("FileCountLimit 默认 = %d", got)
	}
	if got := in.FileBytesLimit(); got != int64(defaultInputMaxFileMB)<<20 {
		t.Errorf("FileBytesLimit 默认 = %d", got)
	}
	if got := in.TotalBytesLimit(); got != int64(defaultInputMaxTotalMB)<<20 {
		t.Errorf("TotalBytesLimit 默认 = %d", got)
	}

	var w Workflow
	if got := w.Parallel(); got != 1 {
		t.Errorf("Parallel 默认 = %d", got)
	}
	if got := w.ProviderConcurrency(); got != defaultProviderMaxReq {
		t.Errorf("ProviderConcurrency 默认 = %d", got)
	}
	w.RetryBackoffMS = 100
	if got := w.CallRetryMax(); got != 2 {
		t.Errorf("配置退避时 CallRetryMax 应默认 2，得到 %d", got)
	}
	w.RetryMax = 5
	if got := w.CallRetryMax(); got != 5 {
		t.Errorf("显式 RetryMax 优先，得到 %d", got)
	}

	var c Cache
	if got := c.CacheTTLHours(); got != defaultCacheTTLHours {
		t.Errorf("CacheTTLHours 默认 = %d", got)
	}
	if got := c.CacheMaxRows(); got != defaultCacheMaxRows {
		t.Errorf("CacheMaxRows 默认 = %d", got)
	}

	// Step.CacheEnabled：显式声明优先于全局
	var st Step
	if !st.CacheEnabled(true) || st.CacheEnabled(false) {
		t.Error("未声明 cache 时应跟随全局开关")
	}
	f := true
	st.Cache = &f
	if !st.CacheEnabled(false) {
		t.Error("显式 cache:true 应覆盖全局 false")
	}
}

func TestStepWithDefaultsAndTemp(t *testing.T) {
	zero := 0.5
	def := Step{Provider: "p", Model: "m", MaxTokens: 100, Temperature: &zero}
	s := Step{}.WithDefaults(def)
	if s.Provider != "p" || s.Model != "m" || s.MaxTokens != 100 {
		t.Errorf("默认值未补齐: %+v", s)
	}
	if s.Temperature == nil || *s.Temperature != 0.5 || s.Temp() != 0.5 {
		t.Errorf("Temperature 默认未继承: %+v", s)
	}
	explicit := Step{Temperature: &[]float64{0.1}[0]}.WithDefaults(def)
	if explicit.Temp() != 0.1 {
		t.Errorf("显式 Temperature 应优先，得到 %v", explicit.Temp())
	}
}

func TestFeatureLookup(t *testing.T) {
	c := loadOK(t)
	if _, ok := c.Feature("demo"); !ok {
		t.Error("应能查到 demo")
	}
	if _, ok := c.Feature("nope"); ok {
		t.Error("不存在的 id 不应命中")
	}
}

func TestFeatureRefIsEnabled(t *testing.T) {
	if !(FeatureRef{}).IsEnabled() {
		t.Error("enabled 缺省应视为启用")
	}
	off := false
	if (FeatureRef{Enabled: &off}).IsEnabled() {
		t.Error("enabled:false 不应视为启用")
	}
}

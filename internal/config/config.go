// Package config 将 ai.yml 反序列化为强类型结构体。
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 对应 ai.yml 两个顶级节点：llm 与 llm-stats。
type Config struct {
	LLM      LLM      `yaml:"llm"`
	LLMStats LLMStats `yaml:"llm-stats"`
}

// LLM 对应 `llm:` 前缀的全部配置。
type LLM struct {
	// 全局默认回退值
	BaseURL               string  `yaml:"base-url"`
	APIKey                string  `yaml:"api-key"`
	Temperature           float64 `yaml:"temperature"`
	TimeoutSeconds        int     `yaml:"timeout-seconds"`
	ConnectTimeoutSeconds int     `yaml:"connect-timeout-seconds"`
	MaxTokens             int     `yaml:"max-tokens"`
	DebugRequestLog       bool    `yaml:"debug-request-log"`

	IoLog            IoLog           `yaml:"io-log"`
	StreamOutput     bool            `yaml:"stream-output"`
	ReasoningEnabled bool            `yaml:"reasoning-enabled"`
	DefaultProvider  string          `yaml:"default-provider"`
	Providers        []Provider      `yaml:"providers"`
	Steps            map[string]Step `yaml:"steps"`
}

// IoLog 输入/输出截断日志配置。
type IoLog struct {
	Enabled   bool `yaml:"enabled"`
	HeadChars int  `yaml:"head-chars"`
	TailChars int  `yaml:"tail-chars"`
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

// Step 按审读步骤独立配置，引用 provider + 模型 + 提示词模板。
type Step struct {
	Provider         string  `yaml:"provider"`
	Model            string  `yaml:"model"`
	PromptTemplate   string  `yaml:"prompt-template"`
	Temperature      float64 `yaml:"temperature"`
	MaxTokens        int     `yaml:"max-tokens"`
	ReasoningEnabled *bool   `yaml:"reasoning-enabled"` // nil = 回退全局
}

// LLMStats 调用统计落库配置。
type LLMStats struct {
	Enabled       bool   `yaml:"enabled"`
	DBPath        string `yaml:"db-path"`
	RetentionDays int    `yaml:"retention-days"`
	QueueCapacity int    `yaml:"queue-capacity"`
}

// Load 读取并解析 yml，展开全局回退值与环境变量。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.expand()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// expand 为 provider 补齐全局回退值，展开 ${ENV:default} 形式的 api-key。
func (c *Config) expand() {
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
	for name, st := range g.Steps {
		if st.Provider == "" {
			st.Provider = g.DefaultProvider
		}
		if st.MaxTokens == 0 {
			st.MaxTokens = g.MaxTokens
		}
		g.Steps[name] = st
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

// validate 启动期校验：default-provider 必须存在，各步骤引用的 provider 必须存在。
func (c *Config) validate() error {
	g := &c.LLM
	names := make(map[string]bool, len(g.Providers))
	for _, p := range g.Providers {
		if p.Name == "" {
			return fmt.Errorf("llm.providers: provider 缺少 name")
		}
		if names[p.Name] {
			return fmt.Errorf("llm.providers: provider %q 重复声明", p.Name)
		}
		names[p.Name] = true
	}
	if g.DefaultProvider != "" && !names[g.DefaultProvider] {
		return fmt.Errorf("llm.default-provider %q 不在 providers 列表中", g.DefaultProvider)
	}
	for name, st := range g.Steps {
		if st.Model == "" {
			return fmt.Errorf("llm.steps.%s: 模型无全局默认，必须显式指定 model", name)
		}
		if st.Provider != "" && !names[st.Provider] {
			return fmt.Errorf("llm.steps.%s: 引用的 provider %q 未声明", name, st.Provider)
		}
	}
	if c.LLMStats.Enabled && c.LLMStats.DBPath == "" {
		return fmt.Errorf("llm-stats.enabled=true 但未配置 db-path")
	}
	return nil
}

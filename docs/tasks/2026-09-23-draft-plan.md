---
## 总体方案
用 Go + langchaingo 做 LLM 调用层、langgraphgo（`smallnest/langgraphgo`，支持泛型 `StateGraph[S]` 与条件边）做工作流编排，把 ai.yml 里的 `providers` / `steps` 映射为 Go 结构体，通过统一的 **LLM 工厂** 生成 OpenAI 兼容客户端，用 `StateGraph` 把「视觉 OCR → 翻译 → 审读 → 专家 → 汇总」串成一条 DAG，每个节点独立复用不同 provider+model，全程落 llm-stats。
### 一、技术栈锁定与 API 现状核对
| 组件 | 选型 | 关键确认点 |
|---|---|---|
| LLM 客户端 | `github.com/tmc/langchaingo/llms/openai` | `openai.New(WithBaseURL/WithToken/WithModel/WithHTTPClient)` 均存在；`WithBaseURL` 默认会自动追加 `/v1`（与 Spring AI 行为一致，所以 yml 里的 `base-url` 不要带 `/v1`） |
| 视觉输入 | `llms.BinaryPart(mime, data)` + `llms.TextPart(text)` 塞进 `MessageContent.Parts` | 支持任意 OpenAI 兼容 VLM（Qwen2.5-VL / Qwen3-VL） |
| Token 限制 | `llms.WithMaxTokens` | 对 LM Studio / Ollama 等"老字段"后端，需叠加 `openai.WithLegacyMaxTokensField()` |
| 图编排 | `github.com/smallnest/langgraphgo/graph` | `NewStateGraph[S]` / `AddNode(name, desc, fn)` / `AddEdge(from,to)` / `AddConditionalEdge(from, condFn)` / `Compile` / `Invoke(ctx, state)` |
| 配置 | `gopkg.in/yaml.v3` | 直接映射 yml 层级 |
| 落库 | `modernc.org/sqlite`（纯 Go，无 CGO） | 对应 `llm-stats.db-path` |
| 日志 | `log/slog` | 替代 `debug-request-log` 的手写开关 |
> **补齐第 2 步及其后续步骤**（对应 yml 注释里提到的 `ocr / translate / proofread / expert / analyze`）：采用 **线性主链 + 条件边做重试/短路** 的混合图，每个节点独立声明 provider+model，避免全局默认模型。
### 二、整体架构（分层）
```
┌─────────────────────────────────────────────────┐
│  CLI / HTTP 入口                                 │
├─────────────────────────────────────────────────┤
│  Orchestrator (langgraphgo StateGraph)          │
│    entry → ocr → translate → proofread          │
│              ↓(条件边) retry / skip            │
│         → expert → analyze → END                │
├─────────────────────────────────────────────────┤
│  Node Layer（每个业务节点一个 fn）               │
│   ocrNode │ translateNode │ proofreadNode │ ... │
├─────────────────────────────────────────────────┤
│  LLM Factory（按 provider 缓存 openai.LLM）     │
│   - lmstudio-local → http://localhost:1234     │
│   - ollama-local   → http://localhost:11434    │
│   - openai-cloud   → $OPENAI_API_KEY           │
├─────────────────────────────────────────────────┤
│  Cross-cutting: io-log │ llm-stats(SQLite) │ stream │
└─────────────────────────────────────────────────┘
```
### 三、目录结构（可直接建仓）
```
goharness/
├── cmd/harness/main.go
├── internal/
│   ├── config/config.go          # ai.yml → struct
│   ├── llm/factory.go            # provider → *openai.LLM
│   ├── llm/stream.go             # LlmStreamOutput 累积器
│   ├── graph/state.go            # WorkflowState 定义
│   ├── graph/builder.go          # 组装 StateGraph
│   ├── graph/nodes/ocr.go        # 视觉 OCR（工资单截图）
│   ├── graph/nodes/translate.go
│   ├── graph/nodes/proofread.go
│   ├── graph/nodes/expert.go
│   ├── graph/nodes/analyze.go
│   ├── prompt/templates.go       # 各步骤系统/用户提示词
│   ├── stats/sqlite.go           # llm-stats 落库
│   └── iolog/logger.go           # 截断日志（head/tail-chars）
├── assets/prompts/*.md
├── config/ai.yml
├── go.mod
└── go.sum
```
---
## 分阶段实施计划
### 📌 P0 · 骨架打通（1–2 天）
**目标**：读配置 → 建 provider → 发起一次 VLM 调用 → 打通工资单截图到结构化 JSON。
**步骤**：
1. `go mod init` + 引入依赖：
   ```
   go get github.com/tmc/langchaingo
   go get github.com/smallnest/langgraphgo
   go get gopkg.in/yaml.v3
   go get modernc.org/sqlite
   ```
2. 写 `internal/config/config.go`，把 yml 里的 `llm.providers` / `llm.io-log` / `llm-stats` 全部反序列化。
3. 写 `internal/llm/factory.go`：按 provider.name 缓存 `*openai.LLM`，用 `WithBaseURL/WithToken/WithHTTPClient` 组装。
4. 写最小 OCR 节点：读工资单 PNG → `BinaryPart` → `GenerateContent` → 打印。
5. 跑通一次真实调用（LM Studio 起本地 qwen3-vl-8b）。
   **关键代码：LLM 工厂**
```go
// internal/llm/factory.go
package llm
import (
    "net/http"
    "time"
    "sync"
    "github.com/tmc/langchaingo/llms/openai"
)
type Factory struct {
    mu        sync.RWMutex
    cache     map[string]*openai.LLM
    providers map[string]ProviderCfg
}
type ProviderCfg struct {
    Name    string `yaml:"name"`
    Type    string `yaml:"type"`
    BaseURL string `yaml:"base-url"`
    APIKey  string `yaml:"api-key"`
    TimeoutSeconds         int `yaml:"timeout-seconds"`
    ConnectTimeoutSeconds  int `yaml:"connect-timeout-seconds"`
    MaxTokens              int `yaml:"max-tokens"`
}
func NewFactory(providers []ProviderCfg) *Factory {
    m := make(map[string]ProviderCfg, len(providers))
    for _, p := range providers { m[p.Name] = p }
    return &Factory{cache: map[string]*openai.LLM{}, providers: m}
}
func (f *Factory) Get(providerName, model string) (*openai.LLM, error) {
    f.mu.RLock(); llm, ok := f.cache[providerName+"|"+model]; f.mu.RUnlock()
    if ok { return llm, nil }
    p, ok := f.providers[providerName]
    if !ok { return nil, fmt.Errorf("unknown provider: %s", providerName) }
    httpClient := &http.Client{
        Timeout: time.Duration(p.TimeoutSeconds) * time.Second,
    }
    llm, err := openai.New(
        openai.WithBaseURL(p.BaseURL),   // langchaingo 会自动加 /v1
        openai.WithToken(p.APIKey),
        openai.WithModel(model),
        openai.WithHTTPClient(httpClient),
    )
    if err != nil { return nil, err }
    f.mu.Lock(); f.cache[providerName+"|"+model] = llm; f.mu.Unlock()
    return llm, nil
}
```
**关键代码：视觉 OCR 节点（工资单截图转数据）**
```go
// internal/graph/nodes/ocr.go
package nodes
import (
    "context"
    "os"
    "github.com/tmc/langchaingo/llms"
    "github.com/tmc/langchaingo/schema"
    "github.com/tmc/langchaingo/llms/openai"
    "goharness/internal/llm"
    "goharness/internal/prompt"
)
type Deps struct {
    Factory *llm.Factory
    Provider string  // e.g. "lmstudio-local"
    Model    string  // e.g. "qwen3-vl-8b"
}
func OCR(d Deps) func(ctx context.Context, s State) (State, error) {
    return func(ctx context.Context, s State) (State, error) {
        img, err := os.ReadFile(s.ImagePath)
        if err != nil { return s, err }
        chat, err := d.Factory.Get(d.Provider, d.Model)
        if err != nil { return s, err }
        messages := []llms.MessageContent{
            llms.TextParts(schema.ChatMessageTypeSystem, prompt.OCRSSystem),
            {
                Parts: []llms.ContentPart{
                    llms.BinaryPart("image/png", img),
                    llms.TextPart(prompt.OCRUser),
                },
            },
        }
        resp, err := chat.GenerateContent(ctx, messages,
            llms.WithTemperature(0.1),
            llms.WithMaxTokens(4096),
            openai.WithLegacyMaxTokensField(), // LM Studio/Ollama 兼容
        )
        if err != nil { return s, err }
        s.OCRText = resp.Choices[0].Content
        return s, nil
    }
}
```
---
### 📌 P1 · 工作流编排（2–3 天）
**目标**：把 OCR → 翻译 → 审读 → 专家 → 汇总串成 langgraphgo 的 StateGraph，支持条件边短路和重试。
**关键代码：State 与 Graph**
```go
// internal/graph/state.go
package graph
type State struct {
    ImagePath    string
    OCRText      string
    Structured   map[string]any   // 工资单结构化字段
    Translation  string
    ReviewNotes  []string
    ExpertReport string
    FinalReport  string
    Err          error
    RetryCount   map[string]int
}
// internal/graph/builder.go
package graph
import (
    "github.com/smallnest/langgraphgo/graph"
    "goharness/internal/graph/nodes"
)
func Build(deps nodes.Deps) (*graph.StateRunnable[State], error) {
    g := graph.NewStateGraph[State]()
    g.AddNode("ocr",       "视觉模型提取工资单", nodes.OCR(deps))
    g.AddNode("translate", "翻译 OCR 结果",     nodes.Translate(deps))
    g.AddNode("proofread", "审读 + 校验字段",   nodes.Proofread(deps))
    g.AddNode("expert",    "专家分析",          nodes.Expert(deps))
    g.AddNode("analyze",   "汇总最终报告",      nodes.Analyze(deps))
    g.SetEntryPoint("ocr")
    // 主链
    g.AddEdge("ocr",       "translate")
    g.AddEdge("translate", "proofread")
    g.AddEdge("proofread", "expert")
    g.AddEdge("expert",    "analyze")
    g.AddEdge("analyze",   graph.END)
    // 条件边：proofread 出错或置信度低 → 回退重试 OCR（最多 2 次）
    g.AddConditionalEdge("proofread", func(ctx context.Context, s State) string {
        if s.Err != nil && s.RetryCount["ocr"] < 2 {
            s.RetryCount["ocr"]++
            return "ocr"
        }
        return "expert"
    })
    return g.Compile()
}
```
**main.go 串联**：
```go
// cmd/harness/main.go
package main
import (
    "context"
    "os"
    "goharness/internal/config"
    "goharness/internal/llm"
    g "goharness/internal/graph"
)
func main() {
    cfg, err := config.Load("config/ai.yml")
    must(err)
    factory := llm.NewFactory(cfg.LLM.Providers)
    app, err := g.Build(nodes.Deps{
        Factory:  factory,
        Provider: cfg.LLM.DefaultProvider,
        Model:    "qwen3-vl-8b",
    })
    must(err)
    initState := State{ImagePath: os.Args[1]}
    finalState, err := app.Invoke(context.Background(), initState)
    must(err)
    println(finalState.FinalReport)
}
```
**每个业务节点的实现套路**（以 translate 为例）：
```go
// internal/graph/nodes/translate.go
func Translate(d Deps) func(ctx context.Context, s State) (State, error) {
    return func(ctx context.Context, s State) (State, error) {
        chat, err := d.Factory.Get("lmstudio-local", "qwen3-8b")
        if err != nil { s.Err = err; return s, err }
        resp, err := chat.GenerateContent(ctx,
            []llms.MessageContent{
                llms.TextParts(schema.ChatMessageTypeSystem, prompt.TranslateSystem),
                llms.TextParts(schema.ChatMessageTypeHuman, s.OCRText),
            },
            llms.WithTemperature(0.3),
            llms.WithMaxTokens(4096),
        )
        if err != nil { s.Err = err; return s, err }
        s.Translation = resp.Choices[0].Content
        return s, nil
    }
}
```
> **补齐第 2 步**：`translate` 节点复用同一工厂，仅切换 provider+model+prompt，完全不影响 OCR 侧的视觉模型。
---
### 📌 P2 · 横切能力（2 天）
**目标**：落库、流式输出、错误重试、io-log 全部就绪。
**1. llm-stats 落库（SQLite）**
```go
// internal/stats/sqlite.go
package stats
import (
    "database/sql"
    "time"
    _ "modernc.org/sqlite"
)
type Recorder struct{ db *sql.DB }
func Open(path string) (*Recorder, error) {
    db, err := sql.Open("sqlite", path)
    if err != nil { return nil, err }
    db.Exec(`CREATE TABLE IF NOT EXISTS calls (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        ts DATETIME, provider TEXT, model TEXT,
        prompt_tokens INT, completion_tokens INT,
        latency_ms INT, ok BOOL, err TEXT
    )`)
    return &Recorder{db}, nil
}
func (r *Recorder) Log(provider, model string, pt, ct, latency int, ok bool, errMsg string) {
    r.db.Exec(`INSERT INTO calls VALUES (NULL,?,?,?,?,?,?,?,?)`,
        time.Now(), provider, model, pt, ct, latency, ok, errMsg)
}
```
在 `factory.Get` 返回的 LLM 外再包一层 `instrumentedLLM`，自动记录耗时/Token/错误。
**2. io-log（head/tail 截断）**
```go
// internal/iolog/logger.go
func Truncate(s string, head, tail int) string {
    r := []rune(s)
    if len(r) <= head+tail+20 { return s }
    return string(r[:head]) + "...[省略 " +
        strconv.Itoa(len(r)-head-tail) + " 字符]..." + string(r[len(r)-tail:])
}
```
每次调用前后写入 `slog`，字段对齐 yml 里的 `head-chars/tail-chars`。
**3. 流式输出**
```go
// internal/llm/stream.go
type StreamAccumulator struct {
    mu        sync.Mutex
    Answer    strings.Builder
    Reasoning strings.Builder
}
func (a *StreamAccumulator) Callback(ctx context.Context, chunk []byte) error {
    a.mu.Lock(); defer a.mu.Unlock()
    a.Answer.Write(chunk)
    return nil
}
```
调用时传 `llms.WithStreamingFunc(acc.Callback)` 即可；Qwen3 思考链用 `llms.WithThinking(&llms.ThinkingConfig{ReturnThinking: true})`。
---
### 📌 P3 · 部署与验收（1 天）
- `Dockerfile` 多阶段构建（scratch 最终镜像，约 15MB）
- `docker-compose.yml` 挂载 `config/ai.yml` + `./database/` 目录
- 环境变量：`OPENAI_API_KEY` 走 `${OPENAI_API_KEY:sk-your-key-here}` 默认值
- 验收脚本：跑一张样例工资单 → 断言 `Structured` 里有 `gross_salary`、`net_salary`、`pay_period` 三个关键字段
---
## 🚧 高频踩坑点与绕行方案
| 坑 | 现象 | 绕行 |
|---|---|---|
| `WithBaseURL` 双重 `/v1` | LM Studio 返回 404 | yml 里只写 `http://localhost:1234`，langchaingo 自动加 `/v1` |
| Ollama 不认 `max_completion_tokens` | 返回 `unknown field` | 叠加 `openai.WithLegacyMaxTokensField()` |
| Qwen3-VL 思考链吃光 max_tokens | 返回空 content | OCR 节点关闭思考：`llms.WithThinkingMode(llms.ThinkingModeDisable)`，或把 `max_tokens` 提到 8192 |
| `MessageGraph` vs `StateGraph` 混用 | 类型断言爆炸 | 统一用 `smallnest/langgraphgo` 的泛型 `StateGraph[S]`，放弃 tmc 的 `MessageGraph` |
| 工资单 PDF 截图 DPI 低 | VLM 幻觉 | 预处理：300 DPI 渲染 + 白边裁剪；OCR prompt 加"不要编造字段" |
| SQLite 并发写锁 | `database is locked` | 用单例 `*sql.DB` + `db.SetMaxOpenConns(1)`，或改 WAL 模式 |
| provider 未配置时报错难定位 | 运行时 panic | 启动时 `factory` 预热所有 yml 里的 provider，缺哪个立即报错 |
---
## ✅ 里程碑验收清单
- [ ] P0：`./harness ./sample/payslip.png` 打印出结构化 JSON（含姓名、税前、税后、税号）
- [ ] P1：`proofread` 失败能自动重跑 `ocr`（最多 2 次），日志可见重试链路
- [ ] P2：`./database/pub-audit-ai.db` 有 `calls` 表，30 天前的记录被清理
- [ ] P3：Docker 内运行 + 环境变量注入 OPENAI_API_KEY，无需改代码切换云端/本地
### 下一轮可以直接让 AI Coding 执行的具体指令
1. "生成 `internal/config/config.go`，完整反序列化上面 ai.yml 的 `llm` + `llm-stats` 两个顶级节点"
2. "为 `ocrNode` 写一个 `_test.go`，用 `httptest.Server` mock 一个 OpenAI 兼容的 `/v1/chat/completions` 返回工资单 JSON"
3. "把 `proofread → ocr` 的重试条件边改成 `errors.Is(s.Err, llms.ErrTimeout)` 才触发"

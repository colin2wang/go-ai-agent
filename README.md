# go-ai-agent

基于 LLM 的通用 AI 工作流平台：以 **可编排的多步骤 LLM 流水线** 为核心，将文档理解、翻译、审读、分析等能力组合成 DAG 工作流，提供 REST API 与开箱即用的 Web UI。

**工资单审读（OCR → 翻译 → 审读 → 专家分析 → 汇总）是平台的第一个业务场景**，新场景只需「提示词 + 步骤配置」即可接入，无需改动编排层。

- **后端**：Go + langchaingo（LLM 调用，OpenAI 兼容协议）+ langgraphgo（工作流编排）+ SQLite（调用统计）
- **API**：REST + OpenAPI 文档（swaggo，`/swagger/index.html`）
- **前端**：Vue3 + TypeScript + Vite，构建产物经 `go:embed` 打进单个二进制
- **部署**：`dist\harness.exe` 双击即用——自动选端口、自动打开浏览器

## 核心能力

| 能力 | 说明 |
|---|---|
| 多步骤工作流编排 | langgraphgo `StateGraph`，节点间主链 + 条件边（失败/低置信度自动重试） |
| 多 Provider 接入 | LM Studio / Ollama / OpenAI 兼容云端统一为 provider 声明，按 `provider+model` 缓存客户端 |
| 按步骤独立配置 | 每个步骤（节点）独立指定 provider + model + 提示词 + 温度 + max-tokens，模型无全局默认 |
| 视觉（VLM）支持 | `BinaryPart` 多模态输入，任意 OpenAI 兼容视觉模型可直接使用 |
| 思考模型兼容 | 流式接收 + REASONING/ANSWER 分道累积，思考链与回答分别落日志 |
| 全链路可观测 | io-log 截断日志（UUID 关联输入输出）+ SQLite 调用统计（Token/耗时/成功率，保留期自动清理） |
| 零配置运行 | 无参数启动 Web 模式，自动挑空闲端口并打开浏览器，端口占用自动切换 |

## 快速开始

### 方式一：直接运行（推荐）

```powershell
.\build.ps1          # 构建前端 + 生成 OpenAPI 文档 + 编译到 dist/
cd dist
.\harness.exe        # 无参数：Web 模式，自动挑空闲端口并打开浏览器
```

- Web UI：`http://localhost:<端口>/`
- Swagger：`http://localhost:<端口>/swagger/index.html`

### 方式二：CLI 单图审读（工资单场景）

```powershell
.\harness.exe .\sample\payslip.png          # 第一参数为图片路径即进入 CLI 模式
.\harness.exe .\payslip.png .\my-ai.yml     # 可指定配置文件
```

### 方式三：前端开发模式

```powershell
cd frontend
npm install
npm run dev          # http://localhost:5173，/api 已代理到后端 8080

# 另开终端运行后端
cd dist && .\harness.exe -addr :8080 -no-browser
```

## 前置条件

本地或云端任一 OpenAI 兼容 LLM 服务（默认走 LM Studio）：

- **LM Studio**：加载模型（视觉场景需 VLM，如 `qwen3-vl-8b`），开启服务（默认 `http://localhost:1234`）
- **Ollama**：`http://localhost:11434`
- **云端**：硅基流动 / OneAPI / vLLM / OpenAI 官方等，`api-key` 支持 `${OPENAI_API_KEY:默认值}` 环境变量展开

模型名可用环境变量覆盖：`HARNESS_DEFAULT_MODEL=qwen3-vl-8b`

## 配置（config/ai.yml）

| 配置节 | 说明 |
|---|---|
| `llm.providers` | 多后端端点列表（name / type / base-url / api-key / 超时 / max-tokens），未填字段回退全局默认 |
| `llm.default-provider` | 步骤未指定 provider 时的回退值；模型无全局默认，各步骤必须显式指定 |
| `llm.steps` | **按步骤独立配置**：引用 provider + model + 提示词模板 + temperature + max-tokens，新场景在此声明自己的步骤 |
| `llm.io-log` | 输入/输出截断日志（head-chars / tail-chars；多模态只记文件名与媒体类型） |
| `llm.stream-output` | 统一流式接收（默认开启），关闭回退普通调用 |
| `llm.reasoning-enabled` | 思考模式开关，各步骤可独立覆盖 |
| `llm-stats` | SQLite 调用统计：db-path / retention-days（过期自动清理）/ queue-capacity |

## 添加新业务场景

平台与场景解耦，新增场景三步接入：

1. **提示词**：`internal/prompt/templates.go` 增加该场景各步骤的系统/用户提示词（或引用 `assets/prompts/*.md`）
2. **节点**（仅当现有五节点不够用时）：在 `internal/graph/nodes/` 增加节点函数，签名 `func(ctx, State) (State, error)`
3. **编排与配置**：`internal/graph/builder.go` 注册节点与边；`ai.yml` 的 `llm.steps` 声明各步骤的 provider+model

现有工资单场景的编排：

```
entry → ocr(VLM) → translate → proofread ─┬→ expert → analyze → END
          ↑______ 失败/存疑重试 ≤2 次 ______┘
```

## 架构

```
┌────────────────────────────────────────────────┐
│ 前端 Vue3 (go:embed)   Swagger UI              │
├────────────────────────────────────────────────┤
│ API: POST /api/payslip/review (multipart)      │
├────────────────────────────────────────────────┤
│ Orchestrator (langgraphgo StateGraph)          │
│   业务场景 = 节点 + 边 + 条件边（重试/短路）      │
├────────────────────────────────────────────────┤
│ Node Layer：每个节点独立 provider+model+prompt  │
├────────────────────────────────────────────────┤
│ LLM Factory（provider+model 缓存、统计埋点）     │
├────────────────────────────────────────────────┤
│ 横切：io-log 截断日志 │ llm-stats(SQLite WAL) │ stream │
└────────────────────────────────────────────────┘
```

### 目录结构

```
├── cmd/harness/          # 入口：main.go（路由分发）/ web.go / cli.go
├── internal/
│   ├── api/              # REST 服务 + Swagger 注解 + 内嵌前端（webdist/）
│   ├── config/           # ai.yml → struct（含 ${ENV:default}、启动期校验）
│   ├── llm/              # 工厂、流式累积器、统计埋点包装
│   ├── graph/            # StateGraph 组装 + 条件边重试（场景编排层）
│   │   └── nodes/        # 业务节点 + State 定义
│   ├── prompt/           # 各步骤提示词模板（场景提示词层）
│   ├── stats/            # SQLite 落库 + 保留期清理
│   └── iolog/            # head/tail 截断日志
├── docs/                 # swag 生成的 OpenAPI 文档
├── frontend/             # Vue3 + TS + Vite
├── config/ai.yml         # 运行配置
└── build.ps1             # 一键构建（swag → npm → go → dist/）
```

## API 摘要

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/payslip/review` | multipart 上传工资单图片（字段 `file`），同步返回审读结果 |
| GET | `/api/jobs/{id}` | 按任务 ID 查询结果（预留异步扩展） |
| GET | `/api/health` | 健康检查 |
| GET | `/swagger/index.html` | OpenAPI 交互文档 |

工资单场景响应字段：`final_report`（汇总报告）、`structured`（结构化工资单字段）、`ocr_text`、`translation`、`review_notes`（审读问题）、`duration_ms`。

## 构建（build.ps1）

```powershell
.\build.ps1                  # 完整流程：测试 → swag → npm build → 嵌入 → 编译
.\build.ps1 -SkipTests       # 跳过 go test
.\build.ps1 -Version v1.0.0  # 注入版本号（harness version 可查看）
```

产物为自包含的 `dist/`：`harness.exe + config/`，前端已嵌入二进制；`database/` 首次运行自动创建。

## 常见问题

| 问题 | 处理 |
|---|---|
| 思考型模型（qwen3）返回空内容 | yml 里把 `max-tokens` 提到 8192，或设 `reasoning-enabled: false` |
| LM Studio 404 | `base-url` 不要带 `/v1`，langchaingo 自动追加 |
| Ollama 报 unknown field | 需兼容旧 `max_tokens` 字段的后端叠加 legacy 选项 |
| 端口被占用 | 已自动切换随机端口，以控制台打印的地址为准 |
| 想换模型 | 设环境变量 `HARNESS_DEFAULT_MODEL`，或在 `llm.steps` 按步骤指定 |

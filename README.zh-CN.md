# go-ai-agent

[English](README.md) | 简体中文

![Platform](https://img.shields.io/badge/Platform-Windows-blue) ![Go](https://img.shields.io/badge/Go-1.27+-00ADD8?logo=go&logoColor=white) ![Wails](https://img.shields.io/badge/Wails-v2-red?logo=wails&logoColor=white) ![Vue](https://img.shields.io/badge/Vue-3-4FC08D?logo=vuedotjs&logoColor=white) ![TypeScript](https://img.shields.io/badge/TypeScript-5.7-3178C6?logo=typescript&logoColor=white) ![Vite](https://img.shields.io/badge/Vite-6-646CFF?logo=vite&logoColor=white) ![SQLite](https://img.shields.io/badge/SQLite-pure--Go-003B57?logo=sqlite&logoColor=white) ![pnpm](https://img.shields.io/badge/pnpm-build-F69220?logo=pnpm&logoColor=white)

一个可插拔的 LLM 工作流桌面应用。Vue 3 前端经 `go:embed` 与 Wails AssetServer 内嵌进单个二进制（`harness.exe`）——没有 HTTP 服务层：前端通过 `window.go.main.App.*` 绑定方法直连 Go，聊天流式响应经 Wails 事件推送。`mq` / `cron` / CLI 子命令保留。

内置子功能（每个都是纯配置插件）：

| 子功能 | 说明 |
| --- | --- |
| AI 聊天（`chat`） | 与任意已配置端点多轮对话；模型列表来自 `GET /v1/models`；默认流式显示；视觉模型可上传图片 |
| 工资单解析（`payslip-ocr`） | 上传一张或多张工资单截图，VLM 识别工资字段并合并为一份结构化 JSON |
| 图书目录（`book-toc`） | 从书页中提取目录结构 |
| 字幕翻译（`subtitle-translate`） | 翻译字幕文件 |

## 架构

```
main.go                  唯一构建入口（Wails 要求主包在模块根）；
                         委托 internal/desktop.Run，按第一个参数分派模式
internal/api/            应用核心：app.go 为 Wails 绑定层（App 方法 + 聊天事件 sink）；
                         static.go 导出内嵌前端资产
internal/config/         三层 YAML 配置加载
internal/graph/          基于 langgraphgo StateGraph 的工作流编排；
                         节点位于 internal/graph/nodes，签名为
                         func(ctx, State) (State, error)
internal/llm/            provider 客户端工厂（按 provider+model 缓存）、流式累积器、
                         统计埋点、wire dump
internal/stats/          SQLite 持久化（modernc.org/sqlite 纯 Go 驱动，无 CGO）
internal/tmpl/           Go text/template 渲染；模板在 templates/
web/                     Vue 3 + TypeScript + Vite 前端；web/src/api.ts 是唯一的
                         后端调用层（Wails 绑定 + runtime 事件，无 fetch）
config/                  三层 YAML：config.yml / ai.yml / features/*.yml
```

聊天流式协议：Wails 频道 `chat:<reqID>`，载荷 `{"event": meta|delta|done|error|collect, "data": ...}`。前端在调用 `ChatSend` **之前**先订阅（事件不缓存）。

## 构建

环境要求：Go 1.24+、Node.js + pnpm、Windows。

```powershell
.\build.ps1          # pnpm build -> 复制 web/dist -> internal/api/webdist -> go build -tags desktop,production
.\build.ps1 -Test    # 同时执行 go test
```

常用命令：

```bash
go build ./...       # 后端编译检查
go test ./...        # 后端测试
cd web && pnpm install && pnpm build   # 前端构建（含 vue-tsc -b 类型检查）
wails dev            # 前端热更新开发（依赖根目录 wails.json）
```

`internal/api/webdist` 为生成物——请勿手改；`build.ps1` 会把 `web/dist` 复制过去，供 `go:embed` 打进二进制。

## 配置

`config/` 下三层 YAML：

- **`config.yml`** — 应用元信息、存储、缓存、日志与子功能注册表（`features[]`）。左侧菜单顺序 = 列表顺序。
- **`ai.yml`** — 只描述 AI 端点。providers 为嵌套列表（`providers[].name`），可被任意子功能的任意步骤复用；未填字段回退到文件顶部的全局默认值。
- **`config/features/<id>.yml`** — 每个子功能一个文件：输入声明、工作流声明、各步骤的 `provider` + `model` + 提示词 + 参数。

### 新增子功能

1. 新建 `config/features/<id>.yml`（工作流 + 步骤 + 提示词）。
2. 在 `config.yml` 的 `features[]` 中注册。

仅此两步——零代码改动，除非需要新的节点/工作流类型（须在 `internal/graph/builder.go` 注册，未知工作流类型会报错）。

### 关键约定

- **提示词只写在 YAML**——代码中不硬编码；非本地步骤缺 `system-prompt` 启动校验报错。改提示词/换模型只改 `config/features/*.yml`，无需重编译。
- `ai.yml` 中 provider 的 `base-url` **必须带 `/v1`**——langchaingo 只拼 `/chat/completions`。
- 模型无全局默认：每个步骤必须显式指定 `provider` + `model`。
- api-key 支持 `${ENV_VAR:默认值}` 环境变量展开；模型名可被 `HARNESS_DEFAULT_MODEL` 兜底覆盖。
- 多模态图片统一手工构造 OpenAI `image_url` data URL（绕开 langchaingo BinaryPart 私有序列化）；发送前按 magic bytes 识别 MIME，长边 >1600px 缩放。
- 上传文件统一走 `uploadSource`（文件名 + 字节）：前端 File → base64 → 绑定方法 → 落盘临时目录，体积/数量上限按子功能 input 声明强校验。
- Go 侧导出给前端的方法都在 `internal/api/app.go` 的 App 上；改签名须同步 `web/src/api.ts` 里的类型声明。
- 内置模板可被 `output.template-dir` 指向的外部 `.tpl` 覆盖，无需重编译。

## 存储与缓存

- SQLite（纯 Go 驱动，无 CGO）存储 LLM 调用统计与异步任务记录；保留期可配置（`retention-days`），并有后台自动清理。
- LLM 结果缓存：相同 provider+模型+消息+参数 命中即不再请求端点（对 OCR/提取类确定性任务收益最大）；TTL 与行数上限可配置。

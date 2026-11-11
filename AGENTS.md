# AGENTS.md

本文件为 AI 编程代理提供项目指引。项目结构、构建命令或架构变化时，必须在同一次改动中同步更新本文件。

## 项目概述

Go 后端 + Vue3 前端的 LLM 工作流桌面应用（Go 1.24，Windows 环境）。采用 **Wails v2** 封装：单个二进制 `harness.exe` 内嵌 Vue3 前端（go:embed + Wails AssetServer），无 HTTP 服务层——前端经 `window.go.main.App.*` 绑定方法直连 Go，聊天流式响应经 Wails 事件推送。子命令保留 mq / cron / CLI。

## 构建与验证命令

- 一键构建（pnpm build → go:embed → `go build -tags desktop,production`）：`.\build.ps1`（PowerShell，需 pnpm）；默认不跑测试，加 `-Test` 才执行 `go test`
- 后端编译检查：`go build ./...`；测试：`go test ./...`
- 前端：在 `web/` 下 `pnpm install` → `pnpm build`（含 `vue-tsc -b` 类型检查，是前端的"检查"命令）
- 前端改动需复制 `web/dist` → `internal/api/webdist` 才会被 `go:embed` 打进二进制（由 `build.ps1` 自动完成）；`internal/api/webdist` 是生成物，不手改
- `wails dev` 可用于前端热更新开发（依赖根目录 wails.json）
- 不需要编译 我自己会编译和验证，仅仅告诉我建议的编译的命令

## 架构与目录

- `main.go`（仓库根）：Wails CLI 要求主包在模块根（issue #566），此为唯一构建入口，委托 `internal/desktop.Run`——按第一个参数分派模式（默认 = Wails 桌面应用，见 `internal/desktop/desktop.go`；无 serve/web 模式）
- `internal/api/`：应用核心（原 HTTP 层已移除）。`app.go` 为 Wails 绑定层（App 方法 + 聊天事件 sink）；`static.go` 的 `AssetsFS()` 导出内嵌前端资产；子功能执行 / 对话转发 / 任务管理为核心函数，不再有 handler
- `internal/config/`：三层配置加载
- `internal/graph/`：langgraphgo StateGraph 编排；节点在 `internal/graph/nodes/`，签名 `func(ctx, State) (State, error)`
- `internal/llm/`：provider 客户端工厂（按 provider+model 缓存）、流式累积器、统计埋点、wire dump
- `internal/stats/`（SQLite，modernc.org/sqlite 纯 Go 驱动，无 CGO）、`internal/tmpl/`（Go text/template 渲染，模板在 `templates/`）
- `web/`：Vue3 + TS + Vite；`web/src/api.ts` 是唯一的后端调用层（Wails 绑定 + runtime 事件，无 fetch）
- `config/`：三层 yml（config.yml / ai.yml / features/*.yml）

## 关键约定与注意事项

- **提示词只写在 yml，代码中不硬编码**；非本地步骤缺 `system-prompt` 启动校验报错。改提示词/换模型只改 `config/features/*.yml`，无需重编译
- **新增子功能 = 新建 `config/features/<id>.yml` + 在 `config.yml` 的 `features[]` 注册**，菜单顺序即列表顺序；除非需要新节点类型，零代码改动。新 workflow 类型须在 `internal/graph/builder.go` 注册，默认分支报"未知工作流类型"
- `ai.yml` 的 provider `base-url` 必须带 `/v1`（langchaingo 只拼 `/chat/completions`）
- 模型无全局默认，每个步骤必须显式指定 provider + model
- 多模态图片统一手工构造 OpenAI `image_url` data URL（绕开 langchaingo BinaryPart 私有序列化），发送前按 magic bytes 识别 MIME、长边 >1600px 缩放
- 上传文件统一走 `uploadSource`（文件名 + 字节）：前端 File → base64 → 绑定方法 → 落盘临时目录，体积/数量上限按子功能 input 声明强校验
- 聊天流式事件协议：Wails 频道 `chat:<reqID>`，载荷 `{"event": meta|delta|done|error|collect, "data": ...}`；前端在调用 `ChatSend` 前先订阅（事件不缓存，先订后发不丢）
- Go 侧导出给前端的方法都在 `internal/api/app.go` 的 App 上，改签名须同步 `web/src/api.ts` 里的类型声明
- api-key 支持 `${ENV_VAR:默认值}` 环境变量展开；模型名可被 `HARNESS_DEFAULT_MODEL` 兜底覆盖
- 内置模板可被 `output.template-dir` 指向的外部 `.tpl` 覆盖，无需重编译

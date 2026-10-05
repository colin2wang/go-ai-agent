# AGENTS.md

本文件为 AI 编程代理提供项目指引。项目结构、构建命令或架构变化时，必须在同一次改动中同步更新本文件。

## 项目概述

Go 后端 + Vue3 前端的 LLM 工作流平台（Go 1.24，Windows 环境）。单个二进制 `harness.exe` 内嵌前端与 OpenAPI 文档，子命令分派模式（serve / mq / cron / CLI）。

## 构建与验证命令

- 一键构建（swag → pnpm build → go:embed → go build）：`.\build.ps1`（PowerShell，需 swaggo/pnpm）；默认不跑测试，加 `-Test` 才执行 `go test`
- 后端编译检查：`go build ./...`；测试：`go test ./...`（目前仓库中暂无 `_test.go`）
- 前端：在 `web/` 下 `pnpm install` → `pnpm build`（含 `vue-tsc -b` 类型检查，是前端的"检查"命令）；开发模式 `pnpm dev`（5173，`/api` 代理到后端 8080）
- 前端改动需复制 `web/dist` → `internal/api/webdist` 才会被 `go:embed` 打进二进制（由 `build.ps1` 自动完成）；`internal/api/webdist` 是生成物，不手改
- 不需要编译 我自己会编译和验证，仅仅告诉我建议的编译的命令

## 架构与目录

- `cmd/harness/`：唯一入口，按第一个参数分派模式
- `internal/api/`：REST + swaggo 注解 + 内嵌前端；`internal/config/`：三层配置加载
- `internal/graph/`：langgraphgo StateGraph 编排；节点在 `internal/graph/nodes/`，签名 `func(ctx, State) (State, error)`
- `internal/llm/`：provider 客户端工厂（按 provider+model 缓存）、流式累积器、统计埋点、wire dump
- `internal/stats/`（SQLite，modernc.org/sqlite 纯 Go 驱动，无 CGO）、`internal/tmpl/`（Go text/template 渲染，模板在 `templates/`）
- `web/`：Vue3 + TS + Vite；`config/`：三层 yml（config.yml / ai.yml / features/*.yml）

## 关键约定与注意事项

- **提示词只写在 yml，代码中不硬编码**；非本地步骤缺 `system-prompt` 启动校验报错。改提示词/换模型只改 `config/features/*.yml`，无需重编译
- **新增子功能 = 新建 `config/features/<id>.yml` + 在 `config.yml` 的 `features[]` 注册**，菜单顺序即列表顺序；除非需要新节点类型，零代码改动。新 workflow 类型须在 `internal/graph/builder.go` 注册，默认分支报"未知工作流类型"
- `ai.yml` 的 provider `base-url` 必须带 `/v1`（langchaingo 只拼 `/chat/completions`）
- 模型无全局默认，每个步骤必须显式指定 provider + model
- 多模态图片统一手工构造 OpenAI `image_url` data URL（绕开 langchaingo BinaryPart 私有序列化），发送前按 magic bytes 识别 MIME、长边 >1600px 缩放
- OpenAPI 文档由 `swag init` 从注解生成到 `api/`，不要手改 `api/docs.go`；改 handler 注解后需重新生成
- api-key 支持 `${ENV_VAR:默认值}` 环境变量展开；模型名可被 `HARNESS_DEFAULT_MODEL` 兜底覆盖
- 内置模板可被 `output.template-dir` 指向的外部 `.tpl` 覆盖，无需重编译

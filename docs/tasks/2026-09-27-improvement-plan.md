# go-ai-agent 项目现状分析与优化建议
## 总体评价
项目定位清晰（可编排 LLM 流水线平台）、架构分层合理（配置 / 编排 / LLM / 横切能力解耦）、"配置即子功能"的扩展理念很好，单二进制部署体验佳。但当前处于"单人可用原型"阶段，在**异步能力、并发、安全、可测试性**上存在明显短板，阻碍其走向多用户/生产化。以下按领域展开。
---
## 一、架构与代码质量问题
| # | 现状依据 | 问题 | 建议 |
|---|---|---|---|
| 1 | `internal/api/chat.go` 1154 行 / 13 个类型 | 单文件职责过载（HTTP handler + SSE + 能力探测 + wire 类型混在一起） | 拆为 `chat_handler.go` / `chat_stream.go` / `chat_caps.go` / `chat_dto.go`，能力探测可下沉到 `internal/llm/probe.go`（已有 ProbeResult） |
| 2 | `web/src/views/ChatView.vue` 1461 行 | 前端巨石组件，状态与 UI 耦合 | 拆 `MessageList / MessageBubble / Composer / ModelPicker / ImageAttach` 组件 + `useChat()` composable；FeatureView(832行)、OverviewView(748行) 同理 |
| 3 | `State` 定义散落在 `nodes.go`(306行) 与 `state.go`(9行) | State 是巨型具体 struct，所有业务字段（OCRText/Entries/Structured…）都塞一起，新场景必须改核心 | 改为 `State` 只保留公共字段 + `Data map[string]any` 或按场景定义子 State；节点签名已声明 `func(ctx, State) (State, error)`，建议文档化此契约 |
| 4 | `GET /api/jobs/{id}` 标注"预留异步扩展"，Server 内 `jobs map` 内存缓存 | **所有执行都是同步阻塞**，多页 OCR/多图工资单易超时；重启丢任务；无法看进度 | 落实异步任务表（SQLite）：`jobs(id, feature_id, status, progress, result, error, created_at)`；`POST run` 立即返回 job_id；前端 SSE/轮询进度（"第 n/m 页"） |
| 5 | mq/cron 仅为骨架 | 消息队列与定时任务无实际实现 | 短期建议用 SQLite 做持久化队列 + 表驱动 worker，比引入外部 MQ 更贴合单二进制定位；否则考虑删除骨架避免误导 |
| 6 | 24 个 Go 文件 5344 行，**未见任何 _test.go** | 零测试，重构风险高 | 优先补：`config.Load`（三层解析/校验）、`graph.Build`、iolog 截断、chat SSE 解析器、能力探测 mock；配合 `httptest` 覆盖 handlers |
---
## 二、可靠性与性能
1. **逐页 OCR 串行**：`book-toc` 逐页 VLM 调用是线性流程。建议用 `errgroup` + 每 provider 信号量并发执行，5 页目录可提速 3~4 倍（merge 步骤仍串行）。
2. **重试策略单一**：条件边重试无指数退避，对 429/5xx 应区分处理（限流退避 vs 立即重试 vs 直接失败）。
3. **无 LLM 结果缓存**：相同图片重复提取会重复烧 token。建议按 `hash(provider+model+messages+params)` 建缓存表（可配 TTL、可关闭），对 OCR 类确定性任务收益大。
4. **能力探测缓存易失**：`capsCache` 仅内存，重启即失效需重测。建议持久化到 SQLite 并加 TTL（如 24h），`ProbedAt` 字段已存在，加过期判断即可。
5. **token 预算失控**：多图 + 长提示词易超 context。建议调用前用 tiktoken-go（已在依赖中）估算，超限时提前缩图/裁剪并提示，而非等端点报错。
6. **无并发上限**：多用户同时跑工作流会打爆本地 LM Studio。建议全局 + 每 provider 双层并发限制（semaphore）。
---
## 三、安全（当前完全裸奔）
1. **无任何认证**：所有 API（含文件上传、任意 LLM 调用）无鉴权。局域网尚可，暴露公网即灾难。最低成本方案：`config.yml` 增加 `server.api-tokens: []`，中间件校验 `Authorization: Bearer`；默认仅绑定 `127.0.0.1`（当前默认 `:8000` 监听所有网卡，应改为默认 localhost）。
2. **上传校验不完整**：chat 有 `max-image-mb`，但 workflow 上传（`/api/features/{id}/run`）未见大小/数量限制声明，建议统一到 `input` 声明并在服务端强校验。
3. **API Key 明文落盘**：`${ENV:default}` 已支持，建议文档强化"生产必用环境变量"，并考虑 key 脱敏展示。
4. **XSS 已处理**（DOMPurify）✅，但建议对 `markdown: false` 的原始文本渲染路径也做统一转义审查。
---
## 四、可观测性与运维
1. **缺标准 metrics**：自研 Logger + SQLite 统计仅够内部看板。建议暴露 `/metrics`（Prometheus 格式），复用 `stats.Record` 埋点即可，成本低收益高。
2. **无结构化日志**：建议 slog（Go 1.21+ 标准库）替换自研 Logger，保留 io-log 作为 LLM 专用通道。
3. **无 CI/Lint**：`build.ps1` 含测试但无 CI 保障。建议加 GitHub Actions（golangci-lint + go test + pnpm build + vue-tsc）。
4. **仅 Windows 构建**：`build.ps1` + `.exe` 强绑定 Windows。建议加 Makefile 或 GoReleaser 支持 linux/mac（modernc.org/sqlite 纯 Go 驱动已具备交叉编译条件）。
5. **配置热更新**：改提示词需重启。低成本方案：`POST /api/reload` 重载 features/*.yml；进阶用 fsnotify 监听。
6. **Graceful shutdown 细节**：已有 `ShutdownTimeoutSeconds`，需确认 SSE 长连接能被正确 drain。
---
## 五、LLM 能力层增强
1. **Prompt 模板变量**：当前 `user-prompt` 是纯静态文本。建议在 `Deps.stepPrompt` 里套 `text/template`（tmpl 引擎已存在），支持 `{{.PageCount}}`、`{{.FileName}}` 等运行时变量。
2. **结构化输出强制**：工资单/目录提取依赖"祈祷模型输出合法 JSON"（虽然 merge 失败有回退）。建议 Step 增加 `output-schema:` 字段 → 请求带 `response_format: json_schema`（或 json_object），响应后校验，失败自动进条件边重试——比现在的文本回退更可靠。
3. **缺 embedding 支持**：`llm.providers` 只有 chat 型端点。增加 `type: embedding` 声明，为 RAG 铺路。
4. **缺 Tool/Function Calling**：langchaingo 已内置 tools 支持，工作流层可增加 `tool-loop` 节点类型。
5. **成本可见性**：ai.yml 每 model 可配单价（输入/输出每百万 token），概览页加"成本估算"卡片，复用现有 ModelStat 聚合。
---
## 六、前端体验
1. **无路由 → 无法深链接**：刷新即回首页，无法分享"某个子功能"的链接。建议引入 hash 路由（`#/{featureId}`、`#/chat/{sessionId}`）——可继续不引 vue-router，用 40 行自实现，或干脆引入（成本极低）。
2. **SSE 无重连**：网络抖动后流式中断无恢复。建议断线时提示 + 「重新生成最后一轮」按钮。
3. **长对话无虚拟滚动**：100 条上限暂可接受，若上服务端历史则必须处理。
4. **暗色主题**：styles.css 已全面 CSS 变量化，加 `[data-theme=dark]` 变量覆盖 + 切换开关即可，工作量很小。
5. **无国际化**：文案硬编码中文，若有分发需求建议先抽 `i18n.ts`。
---
## 新功能建议（按优先级）
### P0 —— 高价值、低成本（1~2 周/项）
| 功能 | 说明 |
|---|---|
| **异步任务 + 实时进度** | 补齐 jobs 表，多页执行时 SSE 推送"第 n/m 页"，彻底解决超时痛点 |
| **并行逐页 OCR** | errgroup 并发，配合 provider 限流 |
| **结果缓存层** | 相同输入命中缓存，省 token 省时间 |
| **Prompt 变量 + 单步调试** | features yml 支持模板变量；支持"只跑指定步骤、查看中间 State"，大幅提升调试效率 |
| **输出 JSON Schema 强校验** | 见上节，结构化提取可靠性质变 |
| **暗色主题 + hash 路由** | 前端体验补齐 |
### P1 —— 高价值、中成本（新场景级）
1. **RAG 知识库子功能**：`kind: rag-chat`——文档上传 → 切块 → embedding（新增 embedding provider）→ 向量存储（推荐 `sqlite-vec`，延续单二进制理念）→ 检索增强对话。这是与现有 chat 子功能最自然的组合，也是平台从"提取工具"迈向"知识助手"的关键一步。
2. **Agent 工具调用子功能**：`kind: agent`——声明 tools（HTTP 调用 / 文件读取 / 子工作流调用），ReAct 循环节点。可让"工资单提取"被 Agent 作为工具调用，实现复合任务。
3. **多会话 + 服务端历史**：SQLite `sessions/messages` 表，会话列表/重命名/搜索；图片可存 blob 或对象引用（解决 localStorage 丢图痛点）；会话标题用 LLM 自动生成。
4. **评测集**：`evalsets/*.jsonl`（输入 + 期望字段），一键批量跑工作流，输出字段命中率报表——**这是迭代 prompt 的科学依据**，比肉眼对比强太多。
5. **认证 + API Key 对外开放**：配合已有 Swagger，可让第三方系统直接调用工作流 API。
6. **PDF 原生输入**：`input.kind: pdf`，pdfcpu 提取页面转图后复用现有 OCR 流程，覆盖扫描 PDF 场景。
### P2 —— 探索性
1. **可视化工作流编辑器**：画布拖拽编排 DAG → 导出 features/*.yml（对标 Dify/Flowise）。现有 `/api/features` 已返回 pipeline，先做只读可视化（SVG 渲染流程图，前端已有 chart.ts 基础），再做编辑。
2. **模型路由与降级链**：`fallback: [providerA, providerB]`，主端点失败/超时自动切换；成本感知路由（简单任务走小模型）。
3. **Webhook 通知**：任务完成回调第三方系统，配合异步任务形成闭环。
4. **语音输入/输出**：ASR（whisper 端点）+ TTS，作为 chat 的可选输入输出通道。
5. **人工介入节点（HITL）**：低置信度字段暂停工作流等人工确认后继续——对工资单这类财务数据场景价值很高。
---
## 落地路线图建议
```
第一阶段（夯实底座）：异步任务 + 并发/限流 + 测试补齐 + 认证 + 拆分巨型文件
第二阶段（能力增强）：JSON Schema 输出 + Prompt 变量 + 结果缓存 + 能力探测持久化 + /metrics
第三阶段（场景扩展）：RAG 知识库 → 评测集 → 多会话 → Agent 工具调用
第四阶段（生态）：可视化编排 + Webhook + 多平台构建发布
```
**核心原则**：先补"可靠性/安全"短板再扩场景——当前三个子功能已验证了"配置即功能"的模式，但同步阻塞、零测试、零鉴权这三件事不解决，新增越多场景风险越大。RAG 与评测集是下一阶段最能放大平台价值的功能，建议优先排期。

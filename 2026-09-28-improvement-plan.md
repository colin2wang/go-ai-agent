# go-ai-agent 项目分析与优化建议
## 一、总体评价
这是一个设计成熟度较高的 LLM 工作流平台：**配置驱动的可插拔子功能、单二进制部署、异步任务 + SSE 进度、结果缓存 + 重试退避、三层配置分离**等设计都体现了对实际痛点的思考。Go 约 8.1k 行 + 前端约 5.1k 行的体量控制得不错。以下建议按维度展开，并标注优先级。
---
## 二、值得肯定的设计（保持）
| 设计 | 评价 |
|---|---|
| 子功能配置化（`features/*.yml`） | 业务解耦的关键，新增场景零代码改动 |
| 图片统一构造 `image_url` data URL + magic bytes 识别 MIME + >1600px 缩放 | 精准解决本地 VLM 静默丢图问题 |
| 异步任务 + SQLite 落库 + 重启可查 | 长任务体验好 |
| 单步 `cache: false` 绕过缓存 | 对随机性步骤的处理很务实 |
| 前端无路由/状态库的克制 | 符合当前规模，避免过度设计 |
| 手写 SVG 图表、内联图标 | 零依赖，体积可控 |
---
## 三、优化建议
### 1. 架构与代码组织 🔴 高优先级
**① Server 职责过重**（`server.go` 582 行 / 8 类型）
`Server` 同时承担：HTTP 路由、异步任务管理（`asyncRun`/`startAsyncJob`/`lookupJob`）、能力探测（`probeModelCaps`）、provider 信息、pipelines 注册。建议拆分：
- `JobManager`：任务生命周期（创建/推进/查询/孤儿回收）
- `CapsService`：能力探测 + 缓存
- `ProviderService`：端点连通性/模型列表代理
  **② `Deps` 结构混杂**（`nodes.go` 452 行）
  `Deps` 同时包含配置、依赖（Factory/Engine）和行为方法（`call`/`stepPrompt`）。建议拆为 `StepConfig`（纯配置）+ `StepRuntime`（执行环境），降低节点测试时构造成本。
  **③ 疑似 `State` 重复定义**
  SUMMARY 显示 `internal/graph/state.go`（9 行）与 `internal/graph/nodes/nodes.go` 都声明了 `State`，且字段列表很长（ImagePath/Pages/OCRText/PageOCR/Entries/Structured/ReviewNotes/FinalReport/Err/RetryCount）。建议：
- 核实是否重复定义
- `State` 按 feature 拆分（payslip 的字段与 book-toc 的 `TOCEntry` 混在一个大结构里），或用子结构组合，避免"万能 State"
  **④ Vue 单文件组件过大**
- `ChatView.vue` 1461 行、`FeatureView.vue` 1015 行。建议拆分为组合式函数 + 子组件：
    - ChatView → `useChatHistory` / `useSSE` / `useCapabilityProbe` + `MessageList` / `ImageUploader` / `ProviderPicker`
    - FeatureView → `useFileUpload`（多选/拖拽/粘贴统一入口）+ `ResultTabs`
      **⑤ 轻量路由缺失**
      无路由意味着：URL 不可分享/收藏、刷新回默认视图、浏览器前进后退失效。**不必引入 vue-router**，用 hash 同步即可（如 `#/f/payslip-ocr`、`#/chat`），App.vue 监听 `hashchange`。成本低收益明显。
### 2. LLM 层 🟠 中高优先级
**① 缓存键的健壮性**
- 若键由字符串拼接生成，注意歧义（`"a"+"bc"` 与 `"ab"+"c"` 冲突），建议先 JSON 序列化（结构化，字段有序）再 SHA-256
- 多模态消息中图片 base64 参与哈希：键极长且计算开销大。可对图片单独哈希后在键中放摘要而非全文
- 淘汰策略"按写入时间淘汰"实为 FIFO，热点数据可能被挤出。建议改 **LRU**（命中时更新时间戳即可，成本低）
  **② 429 重试应优先遵循 `Retry-After`**
  目前是固定指数退避 + 抖动；若端点返回 `Retry-After` 头，应优先采用，避免过早重试加剧限流。
  **③ 限流是并发上限而非速率限制**
  `provider-max-requests` 控制并发数，但突发请求仍可能瞬时打满。对限流敏感的端点可增加令牌桶速率限制（RPM）。
  **④ 流式 + 缓存的写入时机**
  流式响应应在累积完成（`StreamAccumulator.AnswerText()` 拿到完整结果）后才写缓存，且 reasoning/answer 分道累积时需明确缓存内容以 answer 为准（或分开缓存），避免缓存"半截"流。
  **⑤ Token 统计精度**
  依赖 tiktoken-go，但 LM Studio/Ollama 的本地模型 tokenizer 不同，统计值会偏差。建议在 UI 上标注"估算值"，或对本地端点优先采用响应中的 `usage` 字段。
  **⑥ 被动能力探测**
  主动探测（1×1 图片、stream 最小请求）消耗 token。可从正常业务调用的响应中**被动回填**能力信息（如出现 `reasoning_content` 即证明推理能力），减少重复探测成本。
  **⑦ 缺少预算控制**
  建议增加按 feature/step/全局的每日 token 预算，超限告警或拒绝执行——对内网多人共用场景很实用。
### 3. 存储层 🟠 中优先级
**① SQLite 单文件多用途的写竞争**
调用统计 + jobs + llm_cache + caps 共库。WAL 模式下写仍串行，异步任务高频 `UPDATE progress` 可能与缓存写入互相阻塞。建议：
- 连接层面：**单写连接 + 多读连接**（`db.SetMaxOpenConns(1)` 写池 + 单独读池）
- 或将 `llm_cache` 独立成文件（缓存可随时丢弃，隔离故障域）
  **② 任务表膨胀与 VACUUM**
  `retention-days` 清理大表会造成文件膨胀。建议清理后按阈值触发 `PRAGMA incremental_vacuum` 或低峰期 `VACUUM`。
  **③ 孤儿任务回收**
  进程重启后 `processing` 状态的任务永远不会再推进。建议启动时扫描非终态任务标记为 `failed`（附"进程重启中断"信息），前端展示更友好。
  **④ 任务查询能力不足**
  `GET /api/jobs?n=50` 无过滤。建议加 `feature` / `status` / 分页参数。
### 4. 安全 🟠 中优先级
| 项 | 现状 | 建议 |
|---|---|---|
| Web UI 无登录 | api-tokens 只保护 `/api/*` | 至少提供可选的 UI 访问口令或 basic auth；明确"仅限可信内网"的文档声明 |
| wire dump 脱敏 | 只说明 base64 记长度 | **明确确保 `Authorization` / `api-key` 请求头脱敏**，debug 模式下也不可完整输出 |
| Swagger | 生产常开 | 加配置开关，默认关闭（生产环境减少攻击面） |
| 图片解码内存 | 缩放前需解码 | 解码前读取图片头声明的宽高并校验（防解码炸弹），超限直接拒绝 |
| CSP | 未见提及 | 加基本 CSP 响应头（`default-src 'self'`），对 DOMPurify 形成第二道防线 |
| Token 管理 | 静态列表 | 支持注释/轮换即可满足内网需求，无需复杂方案 |
### 5. 可观测性 🟡 中低优先级
- **延迟分位数**：概览只有 `AvgLatencyMS`，建议加 P95/P99（SQLite 中按天聚合或保留原始耗时排序计算）
- **结构化日志**：可选 JSON 输出格式，便于接入日志收集
- **请求 ID 贯穿前端**：错误 toast 中展示 trace UUID，方便用户反馈问题时对账 io-log
### 6. 测试缺口 🔴 高优先级
现有测试覆盖 config / 鉴权 / 缓存键 / 模板 / iolog，但关键路径缺失：
| 缺失项 | 建议 |
|---|---|
| 重试/退避逻辑 | mock 依次返回 429→500→200，断言退避序列与最终成功 |
| StreamAccumulator | 分块乱序、跨 chunk 事件边界、REASONING/ANSWER 分道 |
| 任务状态机 | processing→done/failed 转换、并发更新、孤儿回收 |
| SSE 前端解析器 | `api.ts` 手写的 SSE 解析是重灾区（事件跨 chunk、`event:`/`data:` 行、CRLF），建议 Vitest 覆盖 |
| 端到端 | `httptest` 起 mock OpenAI 端点（含 SSE），跑通完整工作流 |
### 7. 前端细节 🟡
- **localStorage 100 条上限**：可迁移至 IndexedDB（存储更大且异步），至少文档说明清理策略
- **能力探测的降级提示**：探测失败维度"回退启发式并标注"——建议前端标注来源（"实测" vs "推断"），避免误导用户
- **图表大数据量**：最近调用流水多时 SVG 节点数增长，注意虚拟滚动或截断
### 8. 配置体验 🟡
- **提示词热重载**：文档强调"改提示词无需编译"但仍需重启。可对 `features/*.yml` 加文件监听或 SIGHUP 热重载（只需重建 `Deps.Steps`，成本低）
- **校验错误友好化**：启动期校验报错建议输出"feature=X step=Y 缺 system-prompt"式定位信息
### 9. 功能演进方向（可选）
1. **工作流可视化**：前端把 `pipeline` 渲染成 DAG 图（当前仅标签列表），配合实时进度高亮当前节点
2. **失败断点续跑**：结合缓存，任务失败重试时跳过已成功的步骤（当前依赖缓存隐式实现，可显式化）
3. **人工校对环节**：OCR → 人工修正 UI → 合并/渲染，对目录识别场景价值大
4. **RAG 文档问答**作为第 4 个子功能：验证平台对"检索节点"这类新节点类型的扩展性
5. **多会话管理**：chat 当前单会话，加会话列表/命名/搜索
---
## 四、优先级路线图建议
| 阶段 | 内容 |
|---|---|
| **P0（立即）** | 测试补齐（重试/流式累积/SSE 解析/任务状态机）；孤儿任务回收；wire dump 脱敏确认；缓存键改 JSON 序列化 |
| **P1（短期）** | Server/Deps/ChatView 拆分；hash 路由；SQLite 读写分离；429 Retry-After；缓存 LRU |
| **P2（中期）** | Swagger 开关、CSP、UI 可选鉴权；提示词热重载；P95/P99；token 预算；任务过滤查询 |
| **P3（演进）** | DAG 可视化、断点续跑、人工校对、RAG 场景、多会话 |
**核心原则**：当前架构的"配置驱动 + 单二进制"是核心竞争力，优化应以**不增加部署复杂度**为前提——优先做测试、健壮性和代码内聚性，谨慎引入新依赖（全文 35 个 Go 文件 30 个依赖，前端的零依赖策略都值得延续）。

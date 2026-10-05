# 新功能扩展建议
## 一、核心推荐（一句话结论）
**优先做四件能直接复用现有架构的事**：批量文档处理 + Excel 导出、RAG 知识库问答、Webhook/定时触发自动化、文档对比审读——这四项分别在"处理量"、"记忆能力"、"触发方式"、"对比场景"上补齐平台短板，且都不需要推翻现有编排层。
---
## 二、功能全景对照表
按"投入产出比"排序，标注复用点和实现成本：
| 功能方向 | 解决什么场景 | 复用的现有能力 | 新增工作 | 成本 |
|---|---|---|---|---|
| **批量文档处理 + Excel/CSV 导出** | 一次处理几十上百份发票/合同/工资单，汇总落表 | 异步任务、`max-parallel` 并发、模板引擎 | 新增 xlsx 导出节点（excelize 库）、批量输入上限扩展 | 🟢 低 |
| **RAG 知识库问答** | 上传多份文档（手册/合同/规范），对话提问有据可查 | 三层配置、DAG 编排、AI聊天界面 | 新增 chunk/embed/retrieve 节点 + 向量库（sqlite-vec） | 🟡 中 |
| **Webhook 触发 + 定时批处理** | 对接 n8n/Zapier、定时扫描目录自动跑任务 | `harness cron`/`mq` 骨架、`POST /api/features/{id}/run` | 激活 cron 子命令、新增 `/api/hooks/{id}` 端点 | 🟢 低 |
| **文档对比审读** | 合同改版对比、论文前后稿差异分析 | DAG 编排、模板渲染、文本输入类型 | 新增 diff 节点 + 对比审读 feature 配置 | 🟡 中 |
| **OCR 结果人工校对** | 目录识别/工资单提取后可编辑修正，修正结果入库 | FeatureView 结果分标签页、SQLite | 前端结果编辑器 + 校对后结果落库接口 | 🟡 中 |
| **多语言翻译审读流水线** | 翻译 → 回译比对 → 质量评分 | 纯配置新增 feature（多步 LLM） | 仅 yml 配置 | 🟢 低 |
| **简历批量筛选排名** | 上传多份简历 + JD，按匹配度排名 | 批量输入、VLM/文本、模板输出 | 新增 ranking 节点 | 🟢 低 |
| **敏感信息脱敏** | 合同/工资单对外分享前自动打码 | VLM 识别 + 图像处理已有 | 新增 mask 节点（文字区域模糊/替换） | 🟡 中 |
| **音频转写 + 会议纪要** | 上传录音 → 转写 → 提取行动项 | 编排层、结构化输出 | 需接入 Whisper 端点，新增 audio 输入类型 | 🟠 较高 |
---
## 三、高价值功能展开
### 🎯 1. 批量文档处理 + Excel 导出 ⭐ 强烈推荐
**场景痛点**：财务/行政每月要处理几十张发票、几百页合同，单次 8 张的上限太保守，且结果只能 Markdown 下载，无法直接进 Excel 透视。
**实现路径**：
- 放宽 `input.max-files`（如 200 张），配合 `max-parallel` 并发逐页处理
- 新增 `aggregate` 节点：收集每页 `structured` 结果，按字段合并为二维表
- 引入 `github.com/xuri/excelize/v2`（纯 Go、无 CGO 依赖，与现有 `modernc.org/sqlite` 风格一致），在 `internal/tmpl` 增加 `xlsx` 模板类型
- 输出声明支持 `extension: xlsx`，`final_report` 可直接下载 Excel
  **延伸**：结合 LLM 缓存，批量上传后失败的页可单独重跑（只重试缺行），无需全量重来。批量 OCR + Excel 导出是当前 IDP（智能文档处理）工具的标配能力。
### 🎯 2. RAG 知识库问答 ⭐ 次优先
**场景痛点**：AI聊天是"无状态"的，员工问"我司报销标准是多少"得不到答案。
**实现路径**：
- 新增第 4 个子功能 `rag-qa`，走 `kind: chat` 但挂载检索链
- 新增节点类型：`chunk`（文档切片）→ `embed`（调 OpenAI 兼容 embedding 接口）→ `retrieve`（向量检索）→ `answer`（带上下文生成）
- 向量存储用 `github.com/philipgura/ncnn` 类 sqlite-vec 扩展或 `pgvector`（若愿引 Postgres）——**坚持 SQLite 单文件原则**，sqlite-vec 是纯 C 嵌入，与现代c.org/sqlite 兼容
- RAG 是当前企业级 Agent 平台的标配能力
  **注意**：embedding 接口与 chat 接口是不同端点，`ai.yml` 的 provider 需新增 `kind: embedding` 类型声明。
### 🎯 3. Webhook 触发 + 定时批处理 ⭐ 立即可做
**场景痛点**：`harness cron` 和 `harness mq` 是空壳骨架，`POST /api/features/{id}/run` 只能从 Web UI 手动触发。
**实现路径**：
- **激活 cron**：cron 子命令扫描监控目录（如 `watch/payslip/`），发现新文件自动提交对应 feature，结果落到 `output/payslip/` 并按日期归档——这是"零 UI 的批处理模式"，对接扫描仪自动上传场景极实用
- **新增 Webhook 端点**：`POST /api/hooks/{featureId}?token=xxx`，n8n/Zapier 通过 HTTP POST 触发，异步返回 `job_id`，复用现有任务查询接口
- **事件回调**：任务完成时 POST 到用户配置的 `callback-url`，形成完整闭环
  事件驱动自动化是 n8n、Zapier 类工具的核心机制，激活后平台能嵌入现有自动化链路。
### 🎯 4. 文档对比审读
**场景痛点**：合同改版前后对比、学位论文初稿 vs 终稿、规章条款修订——靠人眼逐段比对效率极低。
**实现路径**：
- 新增输入类型 `kind: file-pair`（两份文件），accept 支持 `.txt/.md/.docx`
- 新增 `diff` 节点：纯 Go 实现（`github.com/sergi/go-diff`）做文本 diff，产出结构化差异块
- 新增 `review` 节点：把差异块 + 上下文喂给 LLM，提示词指向"评估每处修改的风险/语义变化"
- 模板输出：差异高亮 HTML + LLM 审读意见
  这个功能在法律/合规/学术场景刚需明确，且 `diff` 节点是完全本地无 LLM 调用，成本低。
### 🎯 5. OCR 结果人工校对
**场景痛点**：目录识别可能漏条目、工资单金额可能识别错——用户没有地方修正，只能重新提交。
**实现路径**：
- FeatureView 结果区新增"编辑"模式：目录条目可增删改行、工资单字段可直接改值
- 新增 `PATCH /api/jobs/{id}/result` 接口，保存校对后的最终结果
- SQLite 新增 `corrections` 表记录"原始识别 vs 人工修正"，可作为后续提示词优化的样本数据
  人工校对 + 证据高亮是专业 IDP 平台（如 Nanonets、Rossum）的标配能力，也是 OCR 产品商业化落地的关键环节。
### 🎯 6. 翻译审读流水线（纯配置可实现）
新增 `features/translate-review.yml`，pipeline 为 `translate → back-translate → score`：
- `translate`：源语言 → 目标语言
- `back-translate`：译文回译为源语言
- `score`：LLM 对比原文与回译文本，输出语义保留度评分 + 问题清单
  这就是"回译质量评估"经典方法，全程 LLM 调用，**不需要新增任何节点**，三个 step 写在 yml 即可，是展示"配置驱动"威力的最佳案例。
---
## 四、不建议优先做的
- **音频转写/会议纪要**：需引入 Whisper 端点 + 音频切片/分帧，改动面大，且与现有 VLM 强项不聚焦，建议放后
- **代码审查助手**：开发者有专用工具（Cursor/Copilot），通用性弱
- **多模态 Agent 工具调用**：langgraphgo 已支持，但工具调用会破坏现有"每 step 独立配置"的简洁性，等真实需求再上
---
## 五、最值得优先做的三件事
1. **Webhook + cron 激活**（1-2 天）：骨架已存在，激活后立刻打通与 n8n 等自动化工具的集成，价值/成本比最高
2. **批量处理 + Excel 导出**（3-5 天）：把工资单场景从"单月几张"升级到"全年批量归档"，财务场景立刻可商用
3. **RAG 知识库问答**（1-2 周）：补齐"平台有记忆"这块拼图，与现有 AI聊天形成完整能力矩阵，是平台从"工具"升级为"知识助手"的关键一步
   **一条原则**：所有新功能都应通过新增节点类型 + yml 配置实现，**不动 `internal/graph` 的编排层代码**——这是验证当初"配置驱动"设计是否真的成立的试金石。

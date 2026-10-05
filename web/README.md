# go-ai-agent 前端（web/）

AI 工作流平台的 Web UI：Vue3 + TypeScript + Vite 单页应用。左侧主菜单 + 概览仪表盘 + 可插拔子功能页（上传执行 / AI 对话），
构建产物由仓库根目录的 `build.ps1` 复制到 `internal/api/webdist/`，经 `go:embed` 打进单个 `harness.exe`，由后端直接托管——部署只有一个二进制，无需独立 Web 服务器。

平台整体架构、三层配置与工作流编排见 [../README.md](../README.md)。

## 技术栈

| 依赖 | 版本 | 用途 |
|---|---|---|
| Vue | ^3.5 | 仅用 `createApp` + SFC 组件，**不引入路由/状态库**（单页视图切换由 App.vue 状态驱动） |
| TypeScript | ~5.7 | 全量类型化，接口类型与 Go 端对齐（`src/types.ts`） |
| Vite | ^6 | 开发服务器 + 生产构建（`base: '/'` 默认，产物输出到 `web/dist/`） |
| marked | ^15 | Markdown 渲染（GFM + `breaks`，换行即 `<br>`，适配聊天场景） |
| DOMPurify | ^3.2 | 渲染后的 HTML 消毒，防模型输出携带 XSS |
| vue-tsc | ^2.2 | `pnpm build` 前的全量类型检查（`vue-tsc -b`） |
| vitest | ^3 | 单元测试：`sse.test.ts`（SSE 解析器）+ `api.test.ts`（mock fetch），`pnpm test` 运行；根目录 `build.ps1` 在构建前执行 |

包管理器为 **pnpm**（锁文件 `pnpm-lock.yaml`）。`pnpm-workspace.yaml` 中的 `allowBuilds: esbuild: true`
是 pnpm ≥ 10 的构建脚本白名单：esbuild 需要 install 脚本链接平台二进制，未批准会导致 `pnpm install`
报 `ERR_PNPM_IGNORED_BUILDS` 并中断根目录 `build.ps1`。新增依赖再出现同类提示时，把对应包名加进该列表即可。

## 开发

```powershell
cd web
pnpm install
pnpm dev            # http://localhost:5173，/api 代理到 http://localhost:8080
```

另开终端以后台模式运行后端（代理目标是 8080，所以要显式指定地址）：

```powershell
cd dist
.\harness.exe -addr :8080 -no-browser
```

| 脚本 | 说明 |
|---|---|
| `pnpm dev` | Vite 开发服务器（端口 5173），`vite.config.ts` 将 `/api` 代理到后端 8080，SSE 流式响应可直连调试 |
| `pnpm build` | `vue-tsc -b` 类型检查 + `vite build`，产物在 `web/dist/` |
| `pnpm test` | `vitest run`：SSE 解析器与 `api.ts` 请求封装的单测（无浏览器依赖） |
| `pnpm preview` | 本地预览构建产物 |

## 与后端的集成方式

- **无路由设计**：不用 vue-router。后端 `GET /api/features` 返回已启用子功能清单，App.vue 据 `activeId`
  在三类视图间切换（`overview` / `kind === 'chat'` → ChatView / 其余 → FeatureView），菜单顺序即后端注册表顺序，
  新增子功能前端零改动。
- **同源 API**：生产环境页面与 `/api/*` 同源（都由 harness 托管），所有请求用相对路径，无需 CORS 与地址配置。
- **嵌入与缓存**：`build.ps1` 把 `web/dist/*` 镜像到 `internal/api/webdist/`（`//go:embed all:webdist`）；
  `assets/` 下文件名带内容 hash 可长缓存，`index.html` 不缓存（见 `internal/api/static.go`），
  因此前端改动必须重跑 `build.ps1` 重新编译二进制才会生效。
- **后端不可用**：启动时先探测 `GET /api/health`，失败则整页提示「后端不可用」，不进入业务视图。

## 目录结构

```
web/
├── index.html              # SPA 入口
├── vite.config.ts          # dev 端口 5173 + /api → :8080 代理
├── package.json            # 脚本与依赖
├── pnpm-workspace.yaml     # pnpm 构建脚本白名单（allowBuilds: esbuild）
├── tsconfig.json
└── src/
    ├── main.ts             # createApp(App)
    ├── App.vue             # 壳层：健康探测 + 特性加载 + 视图分派（overview / chat / workflow）
    ├── api.ts              # 全部 REST 封装 + sendChat（SSE 事件派发）
    ├── sse.ts              # SSE 增量解析器（事件跨 chunk / CRLF / 多行 data），纯函数便于单测
    ├── sse.test.ts         # SSE 解析器单测：跨 chunk、CRLF、多行 data、末尾无分隔符等边界
    ├── api.test.ts         # sendChat / runFeatureAsync 单测（mock fetch + ReadableStream）
    ├── types.ts            # 与 Go 端 JSON 对齐的接口类型（单一事实来源）
    ├── chart.ts            # SVG 路径计算（线性缩放 / Catmull-Rom 平滑 / 面积闭合）与数值格式化
    ├── markdown.ts         # marked + DOMPurify 的 renderMarkdown
    ├── styles.css          # 全局主题与通用类（.card/.btn/.tag/.segmented，CSS 变量见下）
    ├── components/
    │   ├── Sidebar.vue     # 主菜单 / 子功能导航 + 后端健康状态灯
    │   ├── MetricCard.vue  # 概览指标卡（数值 + 趋势 + Sparkline）
    │   ├── Sparkline.vue   # 迷你折线
    │   ├── TrendChart.vue  # 概览趋势大图（调用量 / Token / 失败多序列）
    │   └── AppIcon.vue     # 内联 SVG 图标集（无图标库依赖，按 name 取用）
    └── views/
        ├── OverviewView.vue  # 仪表盘：指标卡 / 趋势图 / 端点连通性测试 / 模型用量 / 最近调用
        ├── FeatureView.vue   # 工作流子功能执行页（通用，由 input/output 声明驱动渲染）
        └── ChatView.vue      # AI 对话页（端点/模型选择、能力探测、SSE 流式、图片输入）
```

## 数据流

- 顶层状态集中在 App.vue：`appInfo / features / providers / overview / activeId`，经 props 下发、事件回传
  （如子功能执行完成 `@done` 触发概览刷新、概览页 `@reload` 切换统计天数）；无全局 store。
- `types.ts` 与 Go 端 handler 响应体一一对应（`FeatureInfo`、`JobResult`、`OverviewStats`、`ModelCaps`、
  `ChatCollectResponse` 等），后端字段变更时须同步修改此文件。
- 全部请求走 `api.ts`：普通请求 `fetch + JSON`（统一提取 `{error}` 错误体抛异常）；
  对话发送 `sendChat()` 走 `fetch + ReadableStream`，逐 chunk 交给 `sse.ts` 的增量解析器，
  再按事件名分发 `meta` / `delta` / `done` 回调；
  响应 Content-Type 非 `text/event-stream` 时按一次性 JSON（`ChatCollectResponse`）返回。
- SSE 解析刻意从 `api.ts` 抽出：事件会被网络分片截断在任意位置、服务端也可能用 CRLF 换行
  （早期内联实现只认 `\n\n`，CRLF 下事件会整段丢失）。抽成纯函数后这些边界都能用单测覆盖，
  见 `sse.test.ts`；流结束时还要 `flush()` 一次，取回未以空行结尾的最后一个事件。

## 视图说明

### OverviewView（概览仪表盘）

指标卡（总调用 / 今日 / Token / 平均耗时 / 成功率）、按天趋势图、模型用量表、最近调用流水；
加载时自动对全部 AI 端点做一次连通性探测（`POST /api/providers/check`），支持逐端点/一键手动「测试」，无后台轮询。

### FeatureView（工作流执行页）

按子功能 `input` 声明渲染上传控件：`multiple` 多图选择、拖拽、**Ctrl+V 粘贴**（全局 paste 监听，
按 `accept` MIME 白名单过滤并自动命名「粘贴图片-N」）；多选下多次添加为**追加语义**。
执行 `POST /api/features/{id}/run`（多文件用 `files` 字段），结果分标签页展示：
生成结果 / 目录条目（book-toc 的 `structured.entries`）/ 结构化数据 / 原始 OCR，`final_report` 可按 `output.extension` 下载。
流程链节点标签由 `STEP_META` 映射（ocr / merge / render 等），未知节点回退原名。

### ChatView（AI 对话）

- **端点 / 模型**：provider 下拉 + `GET /api/providers/models` 拉取模型列表（可刷新重拉）；
  选中模型显示「推理 / 视觉」能力 chip，选项文本带 `[推理·视觉]` 后缀（原生 select 无法渲染 chip）。
- **能力探测**：切换模型后调 `GET /api/providers/caps` 真实探测（后端按 provider+model 缓存），
  探测期间先用列表启发式结论；探测失败不阻塞对话。
- **流式**：开关仅表达「默认流式与否」，端点是否支持由后端调用时检测，不支持时自动降级一次性 JSON，
  前端展示 `note` 并自动关闭开关；支持中途「停止生成」（AbortController）。思考链单独折叠展示。
- **图片输入**：仅视觉模型可用——文件选择 / 拖拽 / Ctrl+V 粘贴，前端按 `chat.max_images`、
  `chat.max_image_mb` 预校验（与后端一致），base64 随消息上报；缩略图可点开大图预览（Esc/点遮罩关闭）。
- **Markdown**：assistant 气泡经 renderMarkdown 渲染，「Markdown 渲染」开关可回退原始文本核对源码。
- **meta 行**：回复底部标注耗时、↑prompt / ↓completion tokens、本次使用的端点/模型（发送时快照，不随下拉切换而变）。

## 状态持久化（localStorage）

| Key | 内容 | 说明 |
|---|---|---|
| `go-ai-agent.chat.history.v1` | 对话历史（最近 100 条） | **图片 base64 不写入**（配额极易溢出），刷新后历史气泡仅保留文件名标签、该图不再参与后续轮次 |
| `go-ai-agent.chat.markdown.v1` | `'on' / 'off'` | Markdown 渲染开关偏好，默认开启 |

## 样式约定

- 全局主题在 `styles.css` 以 CSS 变量定义：浅色卡片式（`--bg` 浅灰底 + `--surface` 白卡），
  强调色绿 / 蓝 / 紫 / 橙（`--green/--blue/--purple/--orange` 及对应 `-soft` 底色），圆角 `--radius*`、阴影 `--shadow`。
- 通用组件类：`.card` / `.btn`（含 `.ghost`）/ `.tag` / `.segmented`；组件内样式一律 `<style scoped>`。
- 响应式：视图主网格在 `@media (max-width: 1180px)` 断点降为单列；侧边栏固定 `--sidebar-w: 252px`。
- 图表不引图表库：`chart.ts` 计算坐标点与 SVG 路径（平滑曲线为 Catmull-Rom 转三次贝塞尔），组件仅负责渲染。

## 常见问题

| 问题 | 处理 |
|---|---|
| `pnpm install` 报 `ERR_PNPM_IGNORED_BUILDS` | 把提示中的包名加入 `pnpm-workspace.yaml` 的 `allowBuilds`（true = 信任），见上文技术栈说明 |
| TS2307 找不到模块 / 类型报错 | `node_modules` 缺失或过期，先在 `web/` 执行 `pnpm install` |
| dev 页面报「后端不可用」 | 确认 harness 已以 `-addr :8080` 启动（代理目标写死 8080，默认 8000 需改 `vite.config.ts` 或换启动参数） |
| dev 下对话 SSE 无输出 | SSE 经 Vite 代理一般可透传；若异常先用 `-addr :8080` 启动后端 + 直接访问 8080 端口排查 |
| 改了前端但 exe 里没生效 | 前端嵌入二进制，需重跑根目录 `.\build.ps1` 重新编译 |
| `pnpm test` 提示找不到 `vitest` | vitest 是新增的 devDependency，先在 `web/` 执行 `pnpm install` |

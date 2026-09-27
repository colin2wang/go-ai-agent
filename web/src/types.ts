/** 与 Go 端 API 对齐的类型定义。 */

/** 应用元信息（GET /api/features、/api/overview 的 app 字段）。 */
export interface AppInfo {
  name: string
  title: string
  version: string
}

/** 输入声明：前端据此渲染上传控件（单文件 / 多页），max_* 为服务端强校验上限。 */
export interface FeatureInput {
  kind: string
  multiple: boolean
  accept: string
  hint: string
  /** 单次上传文件数上限。 */
  max_files: number
  /** 单个文件体积上限 MB。 */
  max_file_mb: number
  /** 本次上传总体积上限 MB。 */
  max_total_mb: number
}

/** 输出声明：模板渲染与下载扩展名。 */
export interface FeatureOutput {
  template: string
  template_dir: string
  extension: string
}

/** 图书目录条目（book-toc 的 structured.entries）。 */
export interface TOCEntry {
  level: number
  number: string
  title: string
  page: number
}

/** 子功能对外描述（GET /api/features）。 */
export interface FeatureInfo {
  id: string
  name: string
  description: string
  icon: string
  order: number
  /** 页面类型：workflow（上传执行）| chat（对话）。 */
  kind: string
  /** 工作流节点顺序（展示用）。 */
  pipeline: string[]
  steps: string[]
  input: FeatureInput
  output: FeatureOutput
  chat: FeatureChat
  /** true = 后端启用异步任务（上传后立即返回 job_id，轮询获取进度）。 */
  async: boolean
}

/** 对话子功能配置（GET /api/features 的 chat 字段）。 */
export interface FeatureChat {
  default_provider: string
  stream_default: boolean
  max_history: number
  /** 单条消息最多附带的图片数（仅视觉模型）。 */
  max_images: number
  /** 单张图片体积上限 MB。 */
  max_image_mb: number
}

/** AI 端点描述（不含 api-key）。 */
export interface ProviderInfo {
  name: string
  type: string
  base_url: string
  default: boolean
}

/** 端点连通性状态。 */
export type ProviderStatus =
  | 'ok'
  | 'auth_error'
  | 'http_error'
  | 'unreachable'
  | 'unknown_provider'
  | 'error'

/** 一次端点连通性探测结果（POST /api/providers/check）。 */
export interface ProviderCheck {
  provider: string
  status: ProviderStatus
  reachable: boolean
  http_status?: number
  latency_ms: number
  detail: string
  checked_at: string
}

export interface ProviderCheckResponse {
  results: ProviderCheck[]
}

/** 按天的调用聚合。 */
export interface DayPoint {
  day: string
  calls: number
  tokens: number
  failures: number
}

/** 按 provider+model 的用量聚合。 */
export interface ModelStat {
  provider: string
  model: string
  calls: number
  tokens: number
  avg_latency_ms: number
  failures: number
}

/** 单次调用流水。 */
export interface CallRow {
  ts: string
  provider: string
  model: string
  prompt_tokens: number
  completion_tokens: number
  latency_ms: number
  ok: boolean
  err: string
}

/** 概览统计聚合（GET /api/overview 的 stats 字段）。 */
export interface OverviewStats {
  total_calls: number
  today_calls: number
  total_tokens: number
  avg_latency_ms: number
  /** 0~1 */
  success_rate: number
  last_call_at: string
  daily: DayPoint[]
  models: ModelStat[]
  recent: CallRow[]
}

/** GET /api/features */
export interface FeaturesResponse {
  app: AppInfo
  features: FeatureInfo[]
  providers: ProviderInfo[]
}

/** GET /api/overview */
export interface OverviewResponse {
  app: AppInfo
  providers: ProviderInfo[]
  stats: OverviewStats | null
  days: number
  server_time: string
}

/** 子功能执行结果（POST /api/features/{id}/run、GET /api/jobs/{id}）。 */
export interface JobResult {
  job_id: string
  feature_id?: string
  status: 'pending' | 'processing' | 'running' | 'done' | 'failed'
  /** 已完成步数（逐页 OCR 时为已完成页数）。 */
  progress?: number
  total?: number
  /** 进度描述（如“ocr 2/5：第 2 页完成”）。 */
  detail?: string
  final_report?: string
  structured?: Record<string, unknown>
  ocr_text?: string
  translation?: string
  review_notes?: string[]
  error?: string
  duration_ms: number
  created_at?: string
  updated_at?: string
}

export interface ApiError {
  error: string
}

// ===== 对话（AI Chat）=====

/** 消息附带的图片（视觉模型输入）；data 为不含 "data:image/png;base64," 前缀的纯 base64。 */
export interface ChatImage {
  name: string
  mime: string
  data: string
}

/** 一条对话消息。 */
export interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
  /** 思考链（流式 Reasoning 增量累积，可选展示）。 */
  reasoning?: string
  /** 随消息附带的图片（仅 user）；刷新恢复后 data 为空（不再上报，也不显示缩略图）。 */
  images?: ChatImage[]
  streaming?: boolean
  error?: string
  meta?: string
}

/** 一次对话请求体（POST /api/chat/send）。 */
export interface ChatSendRequest {
  feature_id: string
  provider: string
  model: string
  messages: { role: string; content: string; images?: ChatImage[] }[]
  stream?: boolean
}

/** 模型列表中的单个模型（后端元数据 + 名称启发粗判）。 */
export interface ModelInfo {
  id: string
  vision: boolean
  reasoning: boolean
}

/** provider 模型列表（GET /api/providers/models）。 */
export interface ProviderModels {
  provider: string
  base_url: string
  models: ModelInfo[]
  fetched_at: string
}

/** 流式能力探测结果（GET /api/providers/stream-check）。 */
export interface StreamCheck {
  provider: string
  model: string
  stream_supported: boolean
  detail: string
  latency_ms: number
  checked_at: string
}

/** 模型能力探测结果（GET /api/providers/caps，后端按 provider+model 缓存）。 */
export interface ModelCaps {
  provider: string
  model: string
  vision: boolean
  reasoning: boolean
  stream: boolean
  stream_note?: string
  /** 视觉/推理是否经过真实接口调用验证（false = 启发式兜底结论）。 */
  probed_vlm: boolean
  probed_json: boolean
  heuristic_ok: boolean
  probed_at: string
  /** true = 命中后端缓存，未重新探测。 */
  cached: boolean
}

/** done 事件负载。 */
export interface ChatDoneEvent {
  duration_ms: number
  prompt_tokens: number
  completion_tokens: number
}

/** SSE 事件回调（sendChat 解析后分发）。 */
export interface ChatStreamHandlers {
  onMeta?: (meta: { provider: string; model: string }) => void
  onDelta?: (delta: { type: 'answer' | 'reasoning'; text: string }) => void
  onDone?: (done: ChatDoneEvent) => void
}

/** 非流式完整响应（POST /api/chat/send）；请求了流式但端点不支持时，后端自动降级并携带 stream_fallback/note。 */
export interface ChatCollectResponse {
  provider: string
  model: string
  answer: string
  reasoning: string
  prompt_tokens: number
  completion_tokens: number
  duration_ms?: number
  stream_fallback?: boolean
  note?: string
}

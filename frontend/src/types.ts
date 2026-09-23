/** 与 Go 端 API 对齐的类型定义。 */

/** 应用元信息（GET /api/features、/api/overview 的 app 字段）。 */
export interface AppInfo {
  name: string
  title: string
  version: string
}

/** 输入声明：前端据此渲染上传控件（单文件 / 多页）。 */
export interface FeatureInput {
  kind: string
  multiple: boolean
  accept: string
  hint: string
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
  /** 工作流节点顺序（展示用）。 */
  pipeline: string[]
  steps: string[]
  input: FeatureInput
  output: FeatureOutput
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

/** 子功能执行结果（POST /api/features/{id}/run）。 */
export interface JobResult {
  job_id: string
  feature_id?: string
  status: 'processing' | 'done' | 'failed'
  final_report?: string
  structured?: Record<string, unknown>
  ocr_text?: string
  translation?: string
  review_notes?: string[]
  error?: string
  duration_ms: number
}

export interface ApiError {
  error: string
}

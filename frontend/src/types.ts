/** 后端 API 响应类型（对应 internal/api.JobResult） */

export interface JobResult {
  job_id: string
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

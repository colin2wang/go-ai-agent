import type {
  ChatCollectResponse,
  ChatDoneEvent,
  ChatSendRequest,
  ChatStreamHandlers,
  FeaturesResponse,
  JobResult,
  OverviewResponse,
  ProviderCheck,
  ProviderCheckResponse,
  ProviderModels,
  ModelCaps,
} from './types'
import { createSSEParser, type SSEEvent } from './sse'

async function getJSON<T>(url: string): Promise<T> {
  const res = await fetch(url)
  const data: unknown = await res.json()
  if (!res.ok) {
    throw new Error((data as { error?: string }).error ?? `HTTP ${res.status}`)
  }
  return data as T
}

/** 子功能清单（左侧菜单 + 流程链）。 */
export function fetchFeatures(): Promise<FeaturesResponse> {
  return getJSON<FeaturesResponse>('/api/features')
}

/** 概览统计（默认最近 14 天）。 */
export function fetchOverview(days = 14): Promise<OverviewResponse> {
  return getJSON<OverviewResponse>(`/api/overview?days=${days}`)
}

/**
 * 查询任务（异步模式轮询用）。
 */
export async function fetchJob(id: string): Promise<JobResult> {
  return getJSON<JobResult>(`/api/jobs/${encodeURIComponent(id)}`)
}

/** 异步任务轮询默认间隔 ms（本地推理任务秒级变化，2s 足够且不打扰后端） */
const JOB_POLL_MS = 2000

/**
 * 执行指定子功能工作流（异步）。
 * 后端 accept 202 返回 job_id + processing：随后轮询 /api/jobs/{id}，
 * 每次进度更新回调 onProgress，直到 done/failed 返回终态。
 * 后端未开异步时直接返回完成态（与旧版同步行为兼容，不轮询）。
 */
export async function runFeatureAsync(
  id: string,
  files: File | File[],
  onProgress?: (job: JobResult) => void,
  pollMs: number = JOB_POLL_MS,
): Promise<JobResult> {
  const first = await runFeature(id, files)
  if (first.status === 'done' || first.status === 'failed') return first
  onProgress?.(first)
  for (;;) {
    await new Promise((r) => setTimeout(r, pollMs))
    const job = await fetchJob(first.job_id)
    onProgress?.(job)
    if (job.status === 'done' || job.status === 'failed') return job
  }
}

/**
 * 执行指定子功能工作流（同步等待，兼容旧行为）。
 * 单文件用 file 字段，多页（如扫描版图书目录）用 files 字段，后端同时兼容。
 * signal 支持中断等待（仅断开本次请求，后台异步任务不受影响）。
 */
export async function runFeature(
  id: string,
  files: File | File[],
  opts?: { async?: boolean; signal?: AbortSignal },
): Promise<JobResult> {
  const list = Array.isArray(files) ? files : [files]
  const form = new FormData()
  if (list.length > 1) {
    for (const f of list) form.append('files', f)
  } else {
    form.append('file', list[0])
  }
  const q = opts?.async === true ? '?async=1' : opts?.async === false ? '?async=0' : ''
  const res = await fetch(`/api/features/${encodeURIComponent(id)}/run${q}`, {
    method: 'POST',
    body: form,
    signal: opts?.signal,
  })
  const data: unknown = await res.json()
  if (!res.ok && res.status !== 202) {
    throw new Error((data as { error?: string }).error ?? `HTTP ${res.status}`)
  }
  return data as JobResult
}

/** 工资单场景快捷入口（等价于 runFeature('payslip-ocr', file)）。 */
export function reviewPayslip(file: File): Promise<JobResult> {
  return runFeature('payslip-ocr', file)
}

/**
 * 测试 AI 端点连通性。
 * 不传 provider 则一次并发测试全部端点；仅在页面加载或用户手动点击时调用，不做实时轮询。
 */
export async function checkProviders(provider?: string): Promise<ProviderCheck[]> {
  const res = await fetch('/api/providers/check', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(provider ? { provider } : {}),
  })
  const data: unknown = await res.json()
  if (!res.ok) {
    throw new Error((data as { error?: string }).error ?? `HTTP ${res.status}`)
  }
  return (data as ProviderCheckResponse).results
}

/** 健康检查。 */
export async function health(): Promise<boolean> {
  try {
    const res = await fetch('/api/health')
    return res.ok
  } catch {
    return false
  }
}

// ===== 对话（AI Chat）=====

/** 拉取指定 provider 的模型列表（后端代理 OpenAI 兼容 GET /v1/models）。 */
export async function fetchProviderModels(provider: string): Promise<ProviderModels> {
  const res = await fetch(`/api/providers/models?provider=${encodeURIComponent(provider)}`)
  const data: unknown = await res.json()
  if (!res.ok) {
    throw new Error((data as { error?: string }).error ?? `HTTP ${res.status}`)
  }
  return data as ProviderModels
}

/** 探测指定模型的能力（后端缓存，模型不变时直接返回缓存结果）。 */
export async function fetchModelCaps(provider: string, model: string): Promise<ModelCaps> {
  const res = await fetch(
    `/api/providers/caps?provider=${encodeURIComponent(provider)}&model=${encodeURIComponent(model)}`,
  )
  const data: unknown = await res.json()
  if (!res.ok) {
    throw new Error((data as { error?: string }).error ?? `HTTP ${res.status}`)
  }
  return data as ModelCaps
}

/**
 * 发送对话消息。stream=true 时解析 SSE 事件（meta/delta/done）分发回调；
 * 端点不支持流式时后端自动降级，此时按一次性 JSON 返回 ChatCollectResponse（stream_fallback/note）。
 * signal 支持中途停止生成。
 */
export async function sendChat(
  req: ChatSendRequest,
  handlers: ChatStreamHandlers,
  signal?: AbortSignal,
): Promise<ChatCollectResponse | null> {
  const res = await fetch('/api/chat/send', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
    signal,
  })
  const ctype = res.headers.get('content-type') ?? ''
  if (!res.ok) {
    let msg = `HTTP ${res.status}`
    try {
      const data: unknown = await res.json()
      msg = (data as { error?: string }).error ?? msg
    } catch {
      /* 忽略非 JSON 错误体 */
    }
    throw new Error(msg)
  }
  if (!ctype.includes('text/event-stream')) {
    return (await res.json()) as ChatCollectResponse
  }

  const reader = res.body?.getReader()
  if (!reader) throw new Error('浏览器不支持流式读取')
  const decoder = new TextDecoder()
  // 事件可能跨 chunk、服务端可能用 CRLF，解析交给独立的增量解析器（见 sse.ts）
  const parser = createSSEParser()
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    const events = parser.push(decoder.decode(value, { stream: true }))
    for (const ev of events) dispatchSSEEvent(ev, handlers)
  }
  // 流结束时补解析一次（上游未以空行结尾的最后一个事件）
  for (const ev of parser.flush()) dispatchSSEEvent(ev, handlers)
  return null
}

/** 把一个已解析的 SSE 事件派发给对应回调。 */
function dispatchSSEEvent(ev: SSEEvent, handlers: ChatStreamHandlers): void {
  let payload: unknown
  try {
    payload = JSON.parse(ev.data)
  } catch {
    return // 非法 JSON 片段丢弃，不中断整条流
  }
  if (ev.event === 'meta') handlers.onMeta?.(payload as { provider: string; model: string })
  else if (ev.event === 'delta')
    handlers.onDelta?.(payload as { type: 'answer' | 'reasoning'; text: string })
  else if (ev.event === 'done') handlers.onDone?.(payload as ChatDoneEvent)
  else if (ev.event === 'error') throw new Error((payload as { error?: string }).error ?? '生成失败')
}

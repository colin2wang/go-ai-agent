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
 * 执行指定子功能工作流。
 * 单文件用 file 字段，多页（如扫描版图书目录）用 files 字段，后端同时兼容。
 */
export async function runFeature(id: string, files: File | File[]): Promise<JobResult> {
  const list = Array.isArray(files) ? files : [files]
  const form = new FormData()
  if (list.length > 1) {
    for (const f of list) form.append('files', f)
  } else {
    form.append('file', list[0])
  }
  const res = await fetch(`/api/features/${encodeURIComponent(id)}/run`, {
    method: 'POST',
    body: form,
  })
  const data: unknown = await res.json()
  if (!res.ok) {
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
  let buf = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    // SSE 事件以空行分隔；data 为单行 JSON
    for (;;) {
      const idx = buf.indexOf('\n\n')
      if (idx < 0) break
      const rawEvent = buf.slice(0, idx)
      buf = buf.slice(idx + 2)
      let event = ''
      const dataLines: string[] = []
      for (const line of rawEvent.split('\n')) {
        if (line.startsWith('event:')) event = line.slice(6).trim()
        else if (line.startsWith('data:')) dataLines.push(line.slice(5).trim())
      }
      if (!event || !dataLines.length) continue
      let payload: unknown
      try {
        payload = JSON.parse(dataLines.join('\n'))
      } catch {
        continue
      }
      if (event === 'meta') handlers.onMeta?.(payload as { provider: string; model: string })
      else if (event === 'delta') handlers.onDelta?.(payload as { type: 'answer' | 'reasoning'; text: string })
      else if (event === 'done') handlers.onDone?.(payload as ChatDoneEvent)
      else if (event === 'error') throw new Error((payload as { error?: string }).error ?? '生成失败')
    }
  }
  return null
}

/**
 * 后端调用层（Wails 桌面模式）：全部经 window.go.main.App 绑定方法直连 Go，
 * 聊天流式响应经 window.runtime 事件推送（频道 "chat:<reqID>"），
 * 不再有任何 HTTP 请求。
 */
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

// ===== Wails 运行时与绑定对象的最小类型声明 =====

interface WailsEvents {
  /** 订阅事件，返回解绑函数。适配 Wails v2 的扁平 runtime API（EventsOn/EventsOff）。 */
  on(eventName: string, callback: (...data: unknown[]) => void): () => void
  off(eventName: string): void
}

interface WailsRuntime {
  EventsOn?: WailsEvents['on']
  EventsOff?: (eventName: string, ...additionalEventNames: string[]) => void
  /** 旧版命名空间形式（保留兼容）。 */
  Events?: WailsEvents
}

interface WailsBindings {
  go: {
    api: {
      App: {
        Features(): Promise<FeaturesResponse>
        Overview(days: number): Promise<OverviewResponse>
        Health(): Promise<boolean>
        Job(id: string): Promise<JobResult>
        Jobs(featureID: string, limit: number): Promise<JobResult[]>
        RunFeature(
          featureID: string,
          files: { name: string; data: string }[],
          extra: Record<string, string>,
          asyncOverride: boolean | null,
        ): Promise<JobResult>
        ProviderCheck(provider: string): Promise<ProviderCheckResponse>
        ProviderModels(provider: string): Promise<ProviderModels>
        ModelCaps(provider: string, model: string, force: boolean): Promise<ModelCaps>
        StreamCheck(provider: string, model: string): Promise<unknown>
        ChatSend(reqID: string, req: ChatSendRequest): Promise<ChatSendAck>
        ChatCancel(reqID: string): Promise<void>
      }
    }
  }
  runtime: WailsRuntime
}

/** ChatSend 流式模式的返回（一次性结果则直接带 collect 字段）。 */
interface ChatSendAck {
  mode: 'stream' | 'collect'
  provider?: string
  model?: string
  [key: string]: unknown
}

// 绑定命名空间 = App 结构体所在的 Go 包名（api），见 web/wailsjs/go/api/App.d.ts
function backend(): WailsBindings['go']['api']['App'] {
  return (window as unknown as WailsBindings).go.api.App
}

/**
 * 等待 Wails 绑定就绪：窗口刚打开时 window.go / window.runtime 可能尚未注入，
 * 轮询直到可用（超时返回 false，避免页面一次性报“后端不可用”）。
 */
export async function waitForBackend(timeoutMs = 10000): Promise<boolean> {
  const w = window as unknown as Partial<WailsBindings>
  const deadline = Date.now() + timeoutMs
  for (;;) {
    if (w.go?.api?.App && (w.runtime?.EventsOn || w.runtime?.Events)) return true
    if (Date.now() >= deadline) return false
    await new Promise((r) => setTimeout(r, 100))
  }
}

function events(): WailsEvents {
  const runtime = (window as unknown as WailsBindings).runtime
  // Wails v2 在 window.runtime 上直接挂 EventsOn/EventsOff（无 Events 命名空间）
  if (typeof runtime?.EventsOn === 'function' && typeof runtime?.EventsOff === 'function') {
    return {
      on: (name, cb) => {
        runtime.EventsOn!(name, cb)
        return () => runtime.EventsOff!(name)
      },
      off: (name) => runtime.EventsOff!(name),
    }
  }
  return runtime!.Events!
}

/** File → 纯 base64（不含 data: 前缀）。 */
async function fileToBase64(f: File): Promise<string> {
  const buf = await f.arrayBuffer()
  let binary = ''
  const bytes = new Uint8Array(buf)
  const chunk = 0x8000
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk))
  }
  return btoa(binary)
}

/** 子功能清单（左侧菜单 + 流程链）。 */
export function fetchFeatures(): Promise<FeaturesResponse> {
  return backend().Features()
}

/** 概览统计（默认最近 14 天）。 */
export function fetchOverview(days = 14): Promise<OverviewResponse> {
  return backend().Overview(days)
}

/** 查询任务（异步模式轮询用）。 */
export function fetchJob(id: string): Promise<JobResult> {
  return backend().Job(id)
}

/** 列出子功能最近的任务（不含大结果字段，查看完整结果用 fetchJob）。 */
export function fetchJobs(featureID: string, limit = 20): Promise<JobResult[]> {
  return backend().Jobs(featureID, limit)
}

/** 异步任务轮询默认间隔 ms（本地推理任务秒级变化，2s 足够且不打扰后端） */
const JOB_POLL_MS = 2000

/**
 * 执行指定子功能工作流（异步）。
 * 后端启用 persist-jobs 时返回 job_id + processing：随后轮询 Job(id)，
 * 每次进度更新回调 onProgress，直到 done/failed 返回终态。
 * 同步模式直接返回完成态（不轮询）。
 */
export async function runFeatureAsync(
  id: string,
  files: File | File[],
  onProgress?: (job: JobResult) => void,
  pollMs: number = JOB_POLL_MS,
  /** 额外表单字段（如字幕翻译的 target_lang）。 */
  extra?: Record<string, string>,
): Promise<JobResult> {
  const first = await runFeature(id, files, { extra })
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
 * 文件字节以 base64 直传绑定方法；extra 为子功能声明的额外参数（如目标语言）。
 */
export async function runFeature(
  id: string,
  files: File | File[],
  opts?: { extra?: Record<string, string> },
): Promise<JobResult> {
  const list = Array.isArray(files) ? files : [files]
  const uploads = await Promise.all(
    list.map(async (f) => ({ name: f.name || 'upload.png', data: await fileToBase64(f) })),
  )
  return backend().RunFeature(id, uploads, opts?.extra ?? {}, null)
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
  const res = await backend().ProviderCheck(provider ?? '')
  return res.results
}

/** 健康检查（桌面模式下后端随窗口常驻，恒可用）。 */
export async function health(): Promise<boolean> {
  try {
    return await backend().Health()
  } catch {
    return false
  }
}

// ===== 对话（AI Chat）=====

/** 拉取指定 provider 的模型列表（后端代理 OpenAI 兼容 GET /v1/models）。 */
export function fetchProviderModels(provider: string): Promise<ProviderModels> {
  return backend().ProviderModels(provider)
}

/** 探测指定模型的能力（后端缓存，模型不变时直接返回缓存结果）。 */
export function fetchModelCaps(provider: string, model: string): Promise<ModelCaps> {
  return backend().ModelCaps(provider, model, false)
}

/**
 * 发送对话消息。stream=true 时经 Wails 事件分发回调（meta/delta/done）；
 * 端点不支持流式时后端自动降级，经 collect 事件返回 ChatCollectResponse
 * （stream_fallback/note）。signal 支持中途停止生成（ChatCancel）。
 */
export async function sendChat(
  req: ChatSendRequest,
  handlers: ChatStreamHandlers,
  signal?: AbortSignal,
): Promise<ChatCollectResponse | null> {
  const reqID = crypto.randomUUID()
  const channel = `chat:${reqID}`
  let settled = false

  return new Promise<ChatCollectResponse | null>((resolve, reject) => {
    const cleanup = () => {
      if (settled) return
      settled = true
      events().off(channel)
      signal?.removeEventListener('abort', onAbort)
    }
    const fail = (err: unknown) => {
      cleanup()
      reject(err instanceof Error ? err : new Error(String(err)))
    }

    const onAbort = () => {
      void backend().ChatCancel(reqID)
    }
    signal?.addEventListener('abort', onAbort, { once: true })

    // 先订阅再发送：事件早于 ChatSend 返回也不会丢
    events().on(channel, (...args: unknown[]) => {
      const payload = args[0] as { event?: string; data?: unknown } | undefined
      if (!payload?.event) return
      try {
        switch (payload.event) {
          case 'meta':
            handlers.onMeta?.(payload.data as { provider: string; model: string })
            break
          case 'delta':
            handlers.onDelta?.(payload.data as { type: 'answer' | 'reasoning'; text: string })
            break
          case 'done':
            handlers.onDone?.(payload.data as ChatDoneEvent)
            cleanup()
            resolve(null)
            break
          case 'collect': {
            // 非流式一次性结果（含流式降级路径）
            cleanup()
            resolve(payload.data as ChatCollectResponse)
            break
          }
          case 'error':
            fail(new Error((payload.data as { error?: string }).error ?? '生成失败'))
            break
        }
      } catch (err) {
        fail(err)
      }
    })

    void backend()
      .ChatSend(reqID, req)
      .then((ack) => {
        if (ack.mode === 'collect') {
          // 非流式：完整结果直接在返回值里（理论上同时有 collect 事件，取先到者）
          cleanup()
          const { mode: _mode, ...rest } = ack
          resolve(rest as unknown as ChatCollectResponse)
        }
      })
      .catch(fail)
  })
}

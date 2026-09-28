import { afterEach, describe, expect, it, vi } from 'vitest'
import { runFeatureAsync, sendChat } from './api'
import type { ChatSendRequest, ChatStreamHandlers, JobResult } from './types'

/** 测试用上传文件（Node 环境下 File 未必可用，用 Blob 代替即可满足 FormData）。 */
const testFile = new Blob(['test']) as unknown as File

/** 构造一个 SSE 响应：chunks 按网络分片的顺序依次下发。 */
function sseResponse(chunks: string[]): Response {
  const enc = new TextEncoder()
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const c of chunks) controller.enqueue(enc.encode(c))
      controller.close()
    },
  })
  return new Response(stream, { status: 200, headers: { 'content-type': 'text/event-stream' } })
}

/** 构造一个普通 JSON 响应（非流式降级路径）。 */
function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  })
}

const baseReq: ChatSendRequest = {
  feature_id: 'chat',
  provider: 'local',
  model: 'qwen',
  messages: [{ role: 'user', content: '你好' }],
  stream: true,
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('sendChat SSE 解析', () => {
  it('按 meta → delta → done 顺序派发', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        sseResponse([
          'event: meta\ndata: {"provider":"local","model":"qwen"}\n\n',
          'event: delta\ndata: {"type":"answer","text":"你好"}\n\n',
          'event: done\ndata: {"duration_ms":10,"prompt_tokens":1,"completion_tokens":2}\n\n',
        ]),
      ),
    )
    const seen: string[] = []
    const handlers: ChatStreamHandlers = {
      onMeta: (m) => seen.push(`meta:${m.model}`),
      onDelta: (d) => seen.push(`delta:${d.type}:${d.text}`),
      onDone: (d) => seen.push(`done:${d.completion_tokens}`),
    }
    const out = await sendChat(baseReq, handlers)
    expect(out).toBeNull()
    expect(seen).toEqual(['meta:qwen', 'delta:answer:你好', 'done:2'])
  })

  it('事件跨 chunk 边界时被正确拼回（旧实现在此处丢事件）', async () => {
    // 故意把 meta 事件切成 4 段，且第二段正好切断 JSON
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        sseResponse([
          'event: me',
          'ta\ndata: {"provider":"local",',
          '"model":"q',
          'wen"}\n\nevent: done\ndata: {"duration_ms":1,"prompt_tokens":0,"completion_tokens":0}\n\n',
        ]),
      ),
    )
    const seen: string[] = []
    await sendChat(baseReq, {
      onMeta: (m) => seen.push(`meta:${m.model}`),
      onDone: () => seen.push('done'),
    })
    expect(seen).toEqual(['meta:qwen', 'done'])
  })

  it('CRLF 行尾也能解析', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        sseResponse(['event: meta\r\ndata: {"provider":"local","model":"qwen"}\r\n\r\n']),
      ),
    )
    const seen: string[] = []
    await sendChat(baseReq, { onMeta: (m) => seen.push(m.model) })
    expect(seen).toEqual(['qwen'])
  })

  it('流末尾没有空行结尾时也能收到最后一个事件', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        sseResponse(['event: done\ndata: {"duration_ms":5,"prompt_tokens":0,"completion_tokens":0}']),
      ),
    )
    const seen: number[] = []
    await sendChat(baseReq, { onDone: (d) => seen.push(d.duration_ms) })
    expect(seen).toEqual([5])
  })

  it('收到 error 事件时抛出异常', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => sseResponse(['event: error\ndata: {"error":"模型加载失败"}\n\n'])),
    )
    await expect(sendChat(baseReq, {})).rejects.toThrow('模型加载失败')
  })

  it('data 为非法 JSON 时跳过该事件而不中断整条流', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        sseResponse([
          'event: delta\ndata: 不是JSON\n\n',
          'event: done\ndata: {"duration_ms":1,"prompt_tokens":0,"completion_tokens":0}\n\n',
        ]),
      ),
    )
    const done: number[] = []
    await sendChat(baseReq, { onDone: (d) => done.push(d.duration_ms) })
    expect(done).toEqual([1])
  })
})

describe('sendChat 非流式与错误路径', () => {
  it('content-type 非 SSE 时按一次性 JSON 返回', async () => {
    const body = {
      provider: 'local',
      model: 'qwen',
      answer: '答案',
      reasoning: '',
      prompt_tokens: 1,
      completion_tokens: 2,
      stream_fallback: true,
      note: '端点不支持流式',
    }
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse(body)))
    const out = await sendChat(baseReq, {})
    expect(out?.answer).toBe('答案')
    expect(out?.stream_fallback).toBe(true)
  })

  it('HTTP 非 2xx 时抛出后端 error 文案', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse({ error: 'provider 不存在' }, 400)))
    await expect(sendChat(baseReq, {})).rejects.toThrow('provider 不存在')
  })

  it('错误响应体不是 JSON 时抛出 HTTP 状态码', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('Bad Gateway', { status: 502 })),
    )
    await expect(sendChat(baseReq, {})).rejects.toThrow('HTTP 502')
  })
})

describe('runFeatureAsync 轮询', () => {
  it('首响即终态时不轮询', async () => {
    const final: JobResult = { job_id: 'j1', status: 'done', duration_ms: 1, final_report: 'ok' }
    const fetchMock = vi.fn(async () => jsonResponse(final))
    vi.stubGlobal('fetch', fetchMock)
    const out = await runFeatureAsync('payslip-ocr', testFile)
    expect(out.status).toBe('done')
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('processing → running → done 时持续轮询并回传每次进度', async () => {
    const pending: JobResult = { job_id: 'j2', status: 'processing', duration_ms: 0, total: 3 }
    const running: JobResult = { job_id: 'j2', status: 'running', duration_ms: 0, total: 3, progress: 1 }
    const done: JobResult = { job_id: 'j2', status: 'done', duration_ms: 9, total: 3, progress: 3 }
    const responses = [jsonResponse(pending, 202), jsonResponse(running), jsonResponse(done)]
    let i = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => responses[i++]),
    )
    const progress: string[] = []
    const out = await runFeatureAsync(
      'book-toc',
      testFile,
      (j) => progress.push(`${j.status}:${j.progress ?? 0}`),
      0, // 测试用：不等真实轮询间隔
    )
    expect(out.status).toBe('done')
    expect(progress).toEqual(['processing:0', 'running:1', 'done:3'])
  })

  it('任务失败时返回 failed 终态', async () => {
    const pending: JobResult = { job_id: 'j3', status: 'processing', duration_ms: 0 }
    const failed: JobResult = { job_id: 'j3', status: 'failed', duration_ms: 3, error: 'OCR 失败' }
    const responses = [jsonResponse(pending, 202), jsonResponse(failed)]
    let i = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => responses[i++]),
    )
    const out = await runFeatureAsync('book-toc', testFile, undefined, 0)
    expect(out.status).toBe('failed')
    expect(out.error).toBe('OCR 失败')
  })
})

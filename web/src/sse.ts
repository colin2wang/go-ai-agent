/**
 * SSE（Server-Sent Events）增量解析器。
 *
 * 浏览器原生 EventSource 不支持 POST，对话走的是 fetch + 手写解析，因此这里是
 * 「重灾区」：事件可能跨 chunk 到达、服务端可能用 CRLF 换行、单条事件可能带
 * 多行 data。抽成纯函数后便于用 Vitest 覆盖，不再依赖真实网络。
 */

/** 一个完整的 SSE 事件（多行 data 已合并为单个字符串）。 */
export interface SSEEvent {
  event: string
  data: string
}

/** SSE 增量解析器：反复 push 文本块，取出已完整的事件。 */
export interface SSEParser {
  /** 追加一段文本（可能截断在事件中间），返回本段内已完整的事件。 */
  push(chunk: string): SSEEvent[]
  /** 流结束后调用：解析残留的、没有分隔符结尾的最后一个事件。 */
  flush(): SSEEvent[]
}

/** 事件分隔符：空行（LF 或 CRLF）。 */
const EVENT_SEP = /\r\n\r\n|\n\n/
/** 行分隔符。 */
const LINE_SEP = /\r\n|\n|\r/

/**
 * 解析单个事件的原始文本（不含分隔符）。
 * 忽略注释行（以 `:` 开头）与 event/data 之外的字段（id / retry 等）。
 */
function parseEvent(raw: string): SSEEvent | null {
  let event = ''
  const data: string[] = []
  for (const line of raw.split(LINE_SEP)) {
    if (line === '' || line.startsWith(':')) continue
    const idx = line.indexOf(':')
    const field = idx < 0 ? line : line.slice(0, idx)
    let value = idx < 0 ? '' : line.slice(idx + 1)
    if (value.startsWith(' ')) value = value.slice(1) // 规范允许 "data: x" 的空格
    if (field === 'event') event = value
    else if (field === 'data') data.push(value)
  }
  // 缺 event 或 data 的片段无法派发，直接丢弃（与旧实现行为一致）
  if (!event || data.length === 0) return null
  return { event, data: data.join('\n') }
}

/** 创建一个新的 SSE 增量解析器。 */
export function createSSEParser(): SSEParser {
  let buf = ''
  return {
    push(chunk: string): SSEEvent[] {
      buf += chunk
      const out: SSEEvent[] = []
      for (;;) {
        const m = EVENT_SEP.exec(buf)
        if (!m) break
        const raw = buf.slice(0, m.index)
        buf = buf.slice(m.index + m[0].length)
        const ev = parseEvent(raw)
        if (ev) out.push(ev)
      }
      return out
    },
    flush(): SSEEvent[] {
      const rest = buf
      buf = ''
      if (rest.trim() === '') return []
      const ev = parseEvent(rest)
      return ev ? [ev] : []
    },
  }
}

/**
 * 把整段 SSE 文本一次性解析为事件列表（测试与非流式场景用）。
 * 等价于 createSSEParser().push(text) + flush()。
 */
export function parseSSE(text: string): SSEEvent[] {
  const p = createSSEParser()
  const out = p.push(text)
  return out.concat(p.flush())
}

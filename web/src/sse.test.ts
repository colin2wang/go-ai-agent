import { describe, expect, it } from 'vitest'
import { createSSEParser, parseSSE, type SSEEvent } from './sse'

/** 便捷断言：把事件列表压成 "event:data" 数组，便于比对。 */
function flatten(events: SSEEvent[]): string[] {
  return events.map((e) => `${e.event}:${e.data}`)
}

describe('parseSSE', () => {
  it('解析单条 LF 分隔的事件', () => {
    expect(flatten(parseSSE('event: meta\ndata: {"provider":"local"}\n\n'))).toEqual([
      'meta:{"provider":"local"}',
    ])
  })

  it('解析 CRLF 分隔的事件（旧实现在此处会漏解析）', () => {
    expect(flatten(parseSSE('event: done\r\ndata: {"duration_ms":1}\r\n\r\n'))).toEqual([
      'done:{"duration_ms":1}',
    ])
  })

  it('解析同一段内的多条事件', () => {
    const text = 'event: delta\ndata: {"t":"a"}\n\nevent: delta\ndata: {"t":"b"}\n\n'
    expect(flatten(parseSSE(text))).toEqual(['delta:{"t":"a"}', 'delta:{"t":"b"}'])
  })

  it('合并单条事件的多行 data（以 \\n 连接）', () => {
    const out = parseSSE('event: note\ndata: 第一行\ndata: 第二行\n\n')
    expect(out).toHaveLength(1)
    expect(out[0].data).toBe('第一行\n第二行')
  })

  it('多行 data 拼回后仍是合法 JSON', () => {
    // 真实流式场景：一个 JSON 负载被 SSE 拆成多行 data 下发
    const out = parseSSE(
      'event: delta\ndata: {"type":"answer",\ndata: "text":"多行",\ndata: "seq":7}\n\n',
    )
    expect(out).toHaveLength(1)
    expect(JSON.parse(out[0].data)).toEqual({ type: 'answer', text: '多行', seq: 7 })
  })

  it('忽略注释行与无关字段', () => {
    const text = ': keep-alive\nid: 7\nretry: 3000\nevent: meta\ndata: {"provider":"p"}\n\n'
    expect(flatten(parseSSE(text))).toEqual(['meta:{"provider":"p"}'])
  })

  it('丢弃缺少 event 或 data 的片段', () => {
    expect(parseSSE('data: 只有 data\n\n')).toEqual([])
    expect(parseSSE('event: 只有 event\n\n')).toEqual([])
  })

  it('data 值中的冒号不被截断', () => {
    expect(flatten(parseSSE('event: delta\ndata: {"text":"a:b"}\n\n'))).toEqual([
      'delta:{"text":"a:b"}',
    ])
  })

  it('解析末尾没有分隔符的残留事件', () => {
    expect(flatten(parseSSE('event: done\ndata: {"ok":true}'))).toEqual(['done:{"ok":true}'])
  })
})

describe('createSSEParser 增量解析', () => {
  it('事件被切在两个 chunk 中间时仍能正确拼回', () => {
    const p = createSSEParser()
    const a = p.push('event: delta\n')
    const b = p.push('data: {"type":"answer","text":"你')
    const c = p.push('好"}\n\n')
    expect(a).toEqual([])
    expect(b).toEqual([])
    expect(flatten(c)).toEqual(['delta:{"type":"answer","text":"你好"}'])
  })

  it('分隔符本身被切在两个 chunk 中间', () => {
    const p = createSSEParser()
    expect(p.push('event: meta\ndata: {"provider":"p"}\n')).toEqual([])
    expect(flatten(p.push('\nevent: done\ndata: {"x":1}\n\n'))).toEqual([
      'meta:{"provider":"p"}',
      'done:{"x":1}',
    ])
  })

  it('一个 chunk 内携带多个事件', () => {
    const p = createSSEParser()
    const out = p.push('event: a\ndata: 1\n\nevent: b\ndata: 2\n\nevent: c\ndata: 3\n\n')
    expect(flatten(out)).toEqual(['a:1', 'b:2', 'c:3'])
  })

  it('CRLF 与 LF 混用也能正确切分', () => {
    const p = createSSEParser()
    const out = p.push('event: a\r\ndata: 1\r\n\r\nevent: b\ndata: 2\n\n')
    expect(flatten(out)).toEqual(['a:1', 'b:2'])
  })

  it('flush 只在有残留内容时产出事件', () => {
    const p = createSSEParser()
    p.push('event: a\ndata: 1\n\n')
    expect(p.flush()).toEqual([]) // 已完整消费，无残留

    const p2 = createSSEParser()
    p2.push('event: b\ndata: 2')
    expect(flatten(p2.flush())).toEqual(['b:2'])
    expect(p2.flush()).toEqual([]) // 重复 flush 不重复产出
  })

  it('逐字符喂入（最细粒度分片）也能正确还原', () => {
    const p = createSSEParser()
    const events: SSEEvent[] = []
    const full = 'event: delta\ndata: {"text":"中文测试"}\n\n'
    for (const ch of full) events.push(...p.push(ch))
    expect(flatten(events)).toEqual(['delta:{"text":"中文测试"}'])
  })
})

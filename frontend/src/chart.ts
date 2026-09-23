/** 迷你图/趋势图的路径计算与数值格式化工具。 */

export interface Pt {
  x: number
  y: number
}

const round2 = (v: number) => Math.round(v * 100) / 100

/** 把数值序列映射到 [x0,x1] × [y0,y1] 的坐标点（y 轴向下）。 */
export function scale(
  values: number[],
  x0: number,
  x1: number,
  y0: number,
  y1: number,
): Pt[] {
  if (values.length === 0) return []
  const max = Math.max(...values)
  const min = Math.min(...values)
  const span = max - min || 1
  const step = values.length > 1 ? (x1 - x0) / (values.length - 1) : 0
  return values.map((v, i) => ({
    x: round2(x0 + i * step),
    y: round2(y1 - ((v - min) / span) * (y1 - y0)),
  }))
}

/** Catmull-Rom 转三次贝塞尔的平滑折线路径。 */
export function smoothLine(pts: Pt[], tension = 0.18): string {
  if (pts.length === 0) return ''
  if (pts.length === 1) return `M ${pts[0].x} ${pts[0].y}`
  let d = `M ${pts[0].x} ${pts[0].y}`
  for (let i = 0; i < pts.length - 1; i++) {
    const p0 = pts[i - 1] ?? pts[i]
    const p1 = pts[i]
    const p2 = pts[i + 1]
    const p3 = pts[i + 2] ?? p2
    const c1x = p1.x + (p2.x - p0.x) * tension
    const c1y = p1.y + (p2.y - p0.y) * tension
    const c2x = p2.x - (p3.x - p1.x) * tension
    const c2y = p2.y - (p3.y - p1.y) * tension
    d += ` C ${round2(c1x)} ${round2(c1y)}, ${round2(c2x)} ${round2(c2y)}, ${p2.x} ${p2.y}`
  }
  return d
}

/** 在折线下补一条到基线的闭合面积路径。 */
export function areaPath(line: string, pts: Pt[], baseline: number): string {
  if (!line || pts.length < 2) return ''
  const first = pts[0]
  const last = pts[pts.length - 1]
  return `${line} L ${last.x} ${baseline} L ${first.x} ${baseline} Z`
}

/** 紧凑数值：1234 → 1.2k。 */
export function compact(n: number): string {
  if (!Number.isFinite(n)) return '0'
  const abs = Math.abs(n)
  if (abs >= 1e9) return `${(n / 1e9).toFixed(abs >= 1e10 ? 0 : 1)}B`
  if (abs >= 1e6) return `${(n / 1e6).toFixed(abs >= 1e7 ? 0 : 1)}M`
  if (abs >= 1e3) return `${(n / 1e3).toFixed(abs >= 1e4 ? 0 : 1)}k`
  return String(Math.round(n))
}

/** 耗时：850 → 850ms；2400 → 2.4s。 */
export function duration(ms: number): string {
  if (!ms) return '0ms'
  if (ms < 1000) return `${Math.round(ms)}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.floor(ms / 60_000)}m${Math.round((ms % 60_000) / 1000)}s`
}

/** 千分位整数。 */
export function thousand(n: number): string {
  return Math.round(n).toLocaleString('en-US')
}

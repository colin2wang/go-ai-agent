import type {
  FeaturesResponse,
  JobResult,
  OverviewResponse,
  ProviderCheck,
  ProviderCheckResponse,
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

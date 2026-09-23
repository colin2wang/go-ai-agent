import type { JobResult } from './types'

/** 上传工资单图片并同步执行审读流水线 */
export async function reviewPayslip(file: File): Promise<JobResult> {
  const form = new FormData()
  form.append('file', file)

  const res = await fetch('/api/payslip/review', {
    method: 'POST',
    body: form,
  })

  const data = await res.json()
  if (!res.ok) {
    throw new Error((data as { error?: string }).error ?? `HTTP ${res.status}`)
  }
  return data as JobResult
}

/** 健康检查 */
export async function health(): Promise<boolean> {
  try {
    const res = await fetch('/api/health')
    return res.ok
  } catch {
    return false
  }
}

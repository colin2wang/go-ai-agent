// 全局日志 store：模块级单例，切换页面不销毁（App.vue 挂载一次 LogPanel）。
import { ref } from 'vue'

export type LogLevel = 'info' | 'warn' | 'error'

export interface LogEntry {
  ts: string
  level: LogLevel
  msg: string
}

/** 上限条数：超出后丢弃最旧的，避免长时间运行撑爆内存。 */
const MAX = 500

export const logLines = ref<LogEntry[]>([])

export function pushLog(level: LogLevel, msg: string) {
  const ts = new Date().toLocaleTimeString('zh-CN', { hour12: false })
  logLines.value.push({ ts, level, msg })
  if (logLines.value.length > MAX) {
    logLines.value.splice(0, logLines.value.length - MAX)
  }
}

export function clearLog() {
  logLines.value = []
}

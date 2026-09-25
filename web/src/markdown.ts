import { marked } from 'marked'
import DOMPurify from 'dompurify'

// 紧凑、安全的 Markdown 渲染配置：
// - breaks: 单个换行也渲染为 <br>，符合聊天场景直觉
// - gfm: 启用表格、删除线等 GitHub 风格扩展
marked.setOptions({ gfm: true, breaks: true })

/**
 * 将 Markdown 文本渲染为经过消毒的 HTML 字符串。
 * 输出通过 DOMPurify 清洗，避免模型返回内容里的 XSS 风险。
 */
export function renderMarkdown(text: string): string {
  if (!text) return ''
  const html = marked.parse(text, { async: false }) as string
  return DOMPurify.sanitize(html, { ADD_ATTR: ['target', 'rel'] })
}

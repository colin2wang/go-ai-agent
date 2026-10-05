// 主题管理：通过 localStorage 持久化，浏览器端保存，无需后端参与
export type Theme = 'light' | 'terminal'

const STORAGE_KEY = 'harness.theme'

export const themes: { value: Theme; label: string }[] = [
  { value: 'light', label: '浅色' },
  { value: 'terminal', label: '终端' },
]

export function loadTheme(): Theme {
  const saved = localStorage.getItem(STORAGE_KEY)
  return saved === 'terminal' ? 'terminal' : 'light'
}

export function applyTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme
  localStorage.setItem(STORAGE_KEY, theme)
}

import { useEffect, useState } from 'react'

export type Theme = 'system' | 'light' | 'dark'
const KEY = 'taskto.theme'

export function storedTheme(): Theme {
  try {
    const value = localStorage.getItem(KEY)
    if (value === 'light' || value === 'dark') return value
  } catch {}
  return 'system'
}

export const THEME_COLOR = { light: '#faf8f4', dark: '#111110' } as const

function applyThemeColor(theme: Theme) {
  for (const meta of document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')) {
    meta.dataset.systemColor ??= meta.content
    meta.content = theme === 'system' ? meta.dataset.systemColor : THEME_COLOR[theme]
  }
}

export function applyTheme(theme: Theme) {
  const root = document.documentElement
  if (theme === 'system') delete root.dataset.theme
  else root.dataset.theme = theme
  applyThemeColor(theme)
}

export function useTheme(): [Theme, (theme: Theme) => void] {
  const [theme, setTheme] = useState(storedTheme)
  useEffect(() => {
    applyTheme(theme)
    try {
      if (theme === 'system') localStorage.removeItem(KEY)
      else localStorage.setItem(KEY, theme)
    } catch {}
  }, [theme])
  return [theme, setTheme]
}

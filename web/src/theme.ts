// Light, dark, or the system's. The choice is the viewer's, kept in this browser only; without
// one (or without storage) the page follows prefers-color-scheme, in CSS, with no script.

import { useEffect, useState } from 'react'

export type Theme = 'system' | 'light' | 'dark'
const KEY = 'taskto.theme'

export function storedTheme(): Theme {
  try {
    const value = localStorage.getItem(KEY)
    if (value === 'light' || value === 'dark') return value
  } catch {
    // Storage blocked: the system's theme.
  }
  return 'system'
}

// The page's ground in each theme: --color-bg of the default brand, ui/themes/mint.css (tests keep
// them equal), so the browser's own chrome (the address bar, the status bar) matches the page.
// A brand theme is chosen apart from this, by data-brand on <html> (docs/design.md); only the
// default brand ships, so these are its colours.
export const THEME_COLOR = { light: '#faf8f4', dark: '#111110' } as const

// The `theme-color` metas in index.html follow the OS through their media queries, which is right
// for the system's theme only. A picked theme sets every one of them to that theme's colour, and
// the system's puts back what each had; the original is kept on the element itself.
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
    } catch {
      // Not remembered, but applied.
    }
  }, [theme])
  return [theme, setTheme]
}

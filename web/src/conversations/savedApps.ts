import type { AppRef, ChatMessage, ToolUse } from './transcript'

export const LIVE_APPS = 3

export function liveApps(messages: ChatMessage[]): Set<ToolUse> {
  const live = new Set<ToolUse>()
  const seen = new Set<string>()
  for (const m of [...messages].reverse()) {
    for (const t of [...m.tools].reverse()) {
      if (!t.app || seen.has(`${t.app.space}-${t.app.app}`)) continue
      seen.add(`${t.app.space}-${t.app.app}`)
      if (seen.size > LIVE_APPS) return live
      live.add(t)
    }
  }
  return live
}

export function lastSavedApp(messages: ChatMessage[], after = 0): { app: AppRef; save: string } | null {
  for (const m of [...messages].reverse()) {
    for (const t of [...m.tools].reverse()) {
      if (t.app && Number.parseInt(t.key, 10) > after) return { app: t.app, save: t.key }
    }
  }
  return null
}

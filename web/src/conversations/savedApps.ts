// The apps the agent saved in a conversation: which show launched in the transcript, and which
// one it saved last.

import type { AppRef, ChatMessage, ToolUse } from './transcript'

// How many apps a conversation shows launched, the most recently saved: each one launches its
// app on the server, so a conversation that built many doesn't launch them all.
export const LIVE_APPS = 3

// The tool uses that show their app launched: each app at its latest save only, so the one copy
// shown is the app as it is now and its actions leave no stale twin, for the LIVE_APPS most
// recently saved apps. The others link to theirs.
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

// The app the agent saved last in the conversation, and the call that saved it, by where the call
// is in the conversation (its id alone may repeat: a second save with the same id is another
// save). With after, only a save in an entry after that one counts.
export function lastSavedApp(messages: ChatMessage[], after = 0): { app: AppRef; save: string } | null {
  for (const m of [...messages].reverse()) {
    for (const t of [...m.tools].reverse()) {
      if (t.app && Number.parseInt(t.key, 10) > after) return { app: t.app, save: t.key }
    }
  }
  return null
}

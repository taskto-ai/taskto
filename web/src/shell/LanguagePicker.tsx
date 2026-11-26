// The user's language: one the app has, or automatic (the browser's, else the workspace's).
// Saved to their account, so the agent and dictation follow it too. It is picked in the account
// menu (AccountMenu.tsx), whose list is drawn only while it is open: the state of the pick and its
// save live here, in a hook of the menu, which stays mounted, so closing the menu while a pick is
// saved loses nothing.

import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { api } from '../api'
import { automaticLocale, LOCALES, localeName, setPreferences, type T, useLocale } from '../i18n'
import { sameSession } from '../session'
import { MenuGroup, MenuItem } from '../ui/Menu'

export interface LanguagePick {
  // The language shown as picked: a tag, or '' for automatic.
  choice: string
  pick: (tag: string) => Promise<void>
}

// The picks of one session and their saving. One save is in flight at a time: when it ends, the
// latest pick is sent if the account doesn't have it yet, so quick picks can't reach the server out
// of order and leave an older one saved. A pick the server refused is undone, so this browser never
// shows a language the account doesn't have.
//
// It lives as long as the session (session.ts), not as long as the menu: the menu can go and come
// back within one session (the shell is swapped for a push's notification) while a save is in
// flight, and the menu drawn again shows that pick and joins that queue rather than starting
// another from the account's older value. When the session ends the queue stops: a save that ends
// after that changes nothing in this browser and says nothing, as the language is the next
// person's now.
interface Queue {
  same: () => boolean
  // The pick the account has, and the latest one asked for.
  saved: string
  wanted: string
  // The account's value as the menu last saw it (/api/me): a menu drawn again sees the same one,
  // which is nothing new and must not overwrite the save the queue knows was accepted since.
  seen: string
  saving: boolean
  // The menus drawn now, told what to show.
  shown: Set<(tag: string) => void>
  // Told once for each pick the server refused (the latest menu's way of saying so).
  refused: (error: unknown) => void
}

let queue: Queue | null = null

function queueOf(initial: string): Queue {
  if (queue?.same()) return queue
  queue = {
    same: sameSession(),
    saved: initial,
    wanted: initial,
    seen: initial,
    saving: false,
    shown: new Set(),
    refused: () => {},
  }
  return queue
}

function show(q: Queue, tag: string) {
  for (const fn of [...q.shown]) fn(tag)
}

async function save(q: Queue) {
  q.saving = true
  try {
    while (q.same() && q.wanted !== q.saved) {
      const tag = q.wanted
      try {
        await api('/api/me', { method: 'PATCH', body: { locale: tag } })
        q.saved = tag
      } catch (e) {
        if (!q.same()) return
        if (q.wanted !== tag) continue // a later pick replaced it: send that one
        q.wanted = q.saved
        show(q, q.saved)
        await setPreferences({ user: q.saved || null })
        if (q.same()) q.refused(e)
      }
    }
  } finally {
    q.saving = false
  }
}

// `onRefused` is told once for each pick the server refused, after the pick was undone and the app
// is in the language it went back to, so what it says is in that one language.
export function useLanguagePick(initial: string, onRefused: (error: unknown) => void): LanguagePick {
  const q = queueOf(initial)
  const [choice, setChoice] = useState(q.wanted)
  const refused = useRef(onRefused)
  refused.current = onRefused
  useLocale() // re-rendered on a change, so the automatic one's name follows the workspace's default
  useLayoutEffect(() => {
    q.shown.add(setChoice)
    q.refused = (e) => refused.current(e)
    setChoice(q.wanted)
    return () => {
      q.shown.delete(setChoice)
    }
  }, [q])
  // A new value from the server (the account's /api/me again) is the one to show and to go
  // back to, unless a pick is still being saved: that value is from before the pick, which
  // stays the one wanted and shown. A value the account already has (this effect's first run,
  // which can come after a pick made at once, or /api/me confirming the pick just saved) is
  // nothing new: acting on it would undo that pick.
  useEffect(() => {
    if (initial === q.seen) return
    q.seen = initial
    if (initial === q.saved) return
    q.saved = initial
    if (q.saving) return
    q.wanted = initial
    show(q, initial)
  }, [initial, q])
  const pick = async (tag: string) => {
    q.wanted = tag
    show(q, tag)
    await setPreferences({ user: tag || null })
    if (q.same() && !q.saving) await save(q)
  }
  return { choice, pick }
}

// The choices, as radio items of the account menu: automatic, then each language the app has,
// each named in its own language.
export function LanguageItems({ language, t }: { language: LanguagePick; t: T }) {
  const { choice, pick } = language
  // A pick the app has no file for (made through the API, for the agent) is listed too.
  const tags = choice && !LOCALES.includes(choice) ? [...LOCALES, choice] : LOCALES
  return (
    <MenuGroup label={t('language.label')}>
      <MenuItem checked={choice === ''} onSelect={() => void pick('')}>
        {t('language.automatic', { name: localeName(automaticLocale()) })}
      </MenuItem>
      {tags.map((tag) => (
        <MenuItem key={tag} checked={choice === tag} onSelect={() => void pick(tag)}>
          <span lang={tag}>{localeName(tag)}</span>
        </MenuItem>
      ))}
    </MenuGroup>
  )
}

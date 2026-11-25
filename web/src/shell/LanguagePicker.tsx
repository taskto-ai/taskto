// The user's language: one the app has, or automatic (the browser's, else the workspace's).
// Saved to their account, so the agent and dictation follow it too. It is picked in the account
// menu (AccountMenu.tsx), whose list is drawn only while it is open: the state of the pick and its
// save live here, in a hook of the menu, which stays mounted, so closing the menu while a pick is
// saved loses nothing.

import { useEffect, useRef, useState } from 'react'
import { api } from '../api'
import { automaticLocale, LOCALES, localeName, setPreferences, type T, useLocale } from '../i18n'
import { useMounted } from '../mounted'
import { MenuGroup, MenuItem } from '../ui/Menu'

export interface LanguagePick {
  // The language shown as picked: a tag, or '' for automatic.
  choice: string
  pick: (tag: string) => Promise<void>
}

// `onRefused` is told once for each pick the server refused, after the pick was undone and the app
// is in the language it went back to, so what it says is in that one language.
export function useLanguagePick(initial: string, onRefused: (error: unknown) => void): LanguagePick {
  const [choice, setChoice] = useState(initial)
  const refused = useRef(onRefused)
  refused.current = onRefused
  useLocale() // re-rendered on a change, so the automatic one's name follows the workspace's default
  // The pick the account has, and the latest pick asked for. One save is in flight at a
  // time: when it ends, the latest pick is sent if the account doesn't have it yet, so
  // quick picks can't reach the server out of order and leave an older one saved. A pick
  // the server refused is undone, so this browser never shows a language the account
  // doesn't have.
  const saved = useRef(initial)
  const wanted = useRef(initial)
  const saving = useRef(false)
  // The menu goes with the session: a save that ends after that (logged out, the session lost)
  // changes nothing in this browser and says nothing, as the language is the next person's now.
  const mounted = useMounted()
  // A new value from the server (the account's /api/me again) is the one to show and to go
  // back to, unless a pick is still being saved: that value is from before the pick, which
  // stays the one wanted and shown. A value the account already has (this effect's first run,
  // which can come after a pick made at once, or /api/me confirming the pick just saved) is
  // nothing new: acting on it would undo that pick.
  useEffect(() => {
    if (initial === saved.current) return
    saved.current = initial
    if (saving.current) return
    wanted.current = initial
    setChoice(initial)
  }, [initial])
  const save = async () => {
    saving.current = true
    try {
      while (mounted.current && wanted.current !== saved.current) {
        const tag = wanted.current
        try {
          await api('/api/me', { method: 'PATCH', body: { locale: tag } })
          saved.current = tag
        } catch (e) {
          if (!mounted.current) return
          if (wanted.current !== tag) continue // a later pick replaced it: send that one
          wanted.current = saved.current
          setChoice(saved.current)
          await setPreferences({ user: saved.current || null })
          if (mounted.current) refused.current(e)
        }
      }
    } finally {
      saving.current = false
    }
  }
  const pick = async (tag: string) => {
    wanted.current = tag
    setChoice(tag)
    await setPreferences({ user: tag || null })
    if (mounted.current && !saving.current) await save()
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

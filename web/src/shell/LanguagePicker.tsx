import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { api } from '../api'
import { automaticLocale, LOCALES, localeName, setPreferences, type T, useLocale } from '../i18n'
import { sameSession } from '../session'
import { MenuGroup, MenuItem } from '../ui/Menu'

export interface LanguagePick {
  choice: string
  pick: (tag: string) => Promise<void>
}

interface Queue {
  same: () => boolean
  saved: string
  wanted: string
  seen: string
  saving: boolean
  shown: Set<(tag: string) => void>
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
        if (q.wanted !== tag) continue
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

export function useLanguagePick(initial: string, onRefused: (error: unknown) => void): LanguagePick {
  const q = queueOf(initial)
  const [choice, setChoice] = useState(q.wanted)
  const refused = useRef(onRefused)
  refused.current = onRefused
  useLocale()
  useLayoutEffect(() => {
    q.shown.add(setChoice)
    q.refused = (e) => refused.current(e)
    setChoice(q.wanted)
    return () => {
      q.shown.delete(setChoice)
    }
  }, [q])
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

export function LanguageItems({ language, t }: { language: LanguagePick; t: T }) {
  const { choice, pick } = language
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

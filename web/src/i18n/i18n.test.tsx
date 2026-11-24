// @vitest-environment jsdom

import { act, cleanup, render, renderHook, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  browserLocale,
  direction,
  LOCALES,
  localeName,
  match,
  resolve,
  setPreferences,
  t,
  translatorFor,
  useFormat,
  useT,
} from '.'
import en from './locales/en.json'
import ptBR from './locales/pt-BR.json'

afterEach(async () => {
  cleanup()
  await setPreferences({ user: 'en', workspace: null })
})

describe('locales', () => {
  it('has English first, and every file in locales/', () => {
    expect(LOCALES[0]).toBe('en')
    expect(LOCALES).toContain('pt-BR')
  })

  it('names each in its own language', () => {
    expect(localeName('en')).toBe('English')
    expect(localeName('pt-BR')).toBe('Português (Brasil)')
  })

  it('knows which scripts run right to left', () => {
    expect(direction('ar')).toBe('rtl')
    expect(direction('he-IL')).toBe('rtl')
    expect(direction('fa')).toBe('rtl')
    expect(direction('ur')).toBe('rtl')
    expect(direction('pt-BR')).toBe('ltr')
    expect(direction('zh-Hant')).toBe('ltr')
    expect(direction('not a tag')).toBe('ltr')
  })

  it('matches a wanted tag, then one of the same language', () => {
    const available = ['en', 'pt-BR', 'zh-Hant']
    expect(match(['pt-BR'], available)).toBe('pt-BR')
    expect(match(['pt-br'], available)).toBe('pt-BR')
    expect(match(['pt-PT'], available)).toBe('pt-BR')
    expect(match(['pt'], available)).toBe('pt-BR')
    expect(match(['de-DE', 'zh-TW'], available)).toBe('zh-Hant')
    expect(match(['de-DE', 'fr'], available)).toBeNull()
    expect(match([], available)).toBeNull()
  })

  it('prefers one in the same script among those of the same language', () => {
    const chinese = ['en', 'zh-Hans', 'zh-Hant']
    expect(match(['zh-TW'], chinese)).toBe('zh-Hant')
    expect(match(['zh-HK'], chinese)).toBe('zh-Hant')
    expect(match(['zh-CN'], chinese)).toBe('zh-Hans')
    expect(match(['zh'], chinese)).toBe('zh-Hans')
    const serbian = ['en', 'sr-Cyrl', 'sr-Latn']
    expect(match(['sr-Latn-RS'], serbian)).toBe('sr-Latn')
    expect(match(['sr'], serbian)).toBe('sr-Cyrl')
    // With only the other script, the same language still beats none.
    expect(match(['sr-Latn'], ['en', 'sr-Cyrl'])).toBe('sr-Cyrl')
  })

  it("takes the user's pick, then the browser's, then the workspace's, then English", () => {
    const available = ['en', 'pt-BR', 'de']
    expect(resolve({ user: 'de', browser: ['pt-BR'], workspace: 'pt-BR' }, available)).toBe('de')
    expect(resolve({ user: null, browser: ['fr-FR', 'pt-BR'], workspace: 'de' }, available)).toBe('pt-BR')
    expect(resolve({ user: null, browser: ['fr-FR'], workspace: 'de' }, available)).toBe('de')
    expect(resolve({ user: null, browser: ['fr-FR'], workspace: null }, available)).toBe('en')
    // A pick the app no longer has falls through to the rest.
    expect(resolve({ user: 'ja', browser: ['fr-FR'], workspace: 'de' }, available)).toBe('de')
  })
})

describe('messages', () => {
  const catalog = {
    hello: 'Hello, {name}!',
    files_zero: 'No files',
    files_one: '{count} file',
    files_other: '{count} files',
    only_other: '{count} things',
    link: 'Read <a>the docs</a>, {name}.',
    licence: '{what} under <a>{licence}</a>.',
    braces: 'Send {"text": "…"} as {name}',
  }

  it('fills in placeholders, and leaves unknown ones', () => {
    const tr = translatorFor('en', catalog) as unknown as (k: string, p?: object) => string
    expect(tr('hello', { name: 'Alice' })).toBe('Hello, Alice!')
    expect(tr('hello')).toBe('Hello, {name}!')
    expect(tr('braces', { name: 'JSON' })).toBe('Send {"text": "…"} as JSON')
  })

  it('picks the plural form for count, with an exact zero of its own', () => {
    const tr = translatorFor('en', catalog) as unknown as (k: string, p?: object) => string
    expect(tr('files', { count: 0 })).toBe('No files')
    expect(tr('files', { count: 1 })).toBe('1 file')
    expect(tr('files', { count: 1234 })).toBe('1,234 files')
    expect(tr('only', { count: 1 })).toBe('1 things')
  })

  it("formats numbers the locale's way", () => {
    const tr = translatorFor('pt-BR', catalog) as unknown as (k: string, p?: object) => string
    expect(tr('files', { count: 1234 })).toBe('1.234 files')
  })

  it('wraps tagged text with rich params', () => {
    const tr = translatorFor('en', catalog)
    const rich = tr.rich as unknown as (k: string, p: object) => React.ReactNode
    render(<p>{rich('link', { a: (text: React.ReactNode) => <a href="/docs">{text}</a>, name: <b>Alice</b> })}</p>)
    expect(screen.getByRole('link', { name: 'the docs' }).getAttribute('href')).toBe('/docs')
    expect(screen.getByText('Alice').tagName).toBe('B')
    expect(screen.getByRole('paragraph').textContent).toBe('Read the docs, Alice.')
    cleanup()
    // A placeholder inside a tag's text is filled in before the tag wraps it.
    render(
      <p>
        {rich('licence', {
          what: 'Whistle',
          licence: 'Apache-2.0',
          a: (text: React.ReactNode) => <a href="/l">{text}</a>,
        })}
      </p>,
    )
    expect(screen.getByRole('link', { name: 'Apache-2.0' })).toBeTruthy()
    expect(screen.getByRole('paragraph').textContent).toBe('Whistle under Apache-2.0.')
  })

  it('falls back to English for a key the locale lacks, then to the key', () => {
    const tr = translatorFor('pt-BR', {}) as unknown as (k: string, p?: object) => string
    expect(tr('app.tryAgain')).toBe(en.app.tryAgain)
    expect(tr('no.such.key')).toBe('no.such.key')
  })

  it('checks keys when it compiles', () => {
    // @ts-expect-error: not a key in en.json
    expect(t('shell.noSuchKey')).toBe('shell.noSuchKey')
    expect(t('shell.logOut')).toBe('Log out')
  })
})

function Probe() {
  const t = useT()
  const f = useFormat()
  return (
    <p>
      {t('shell.logOut')} | {f.number(1234.5)} | {f.date('2026-11-19T12:00:00Z')}
    </p>
  )
}

describe('switching', () => {
  it("loads a locale when picked, re-renders, and sets the page's lang and dir", async () => {
    render(<Probe />)
    expect(screen.getByRole('paragraph').textContent).toMatch(/^Log out \| 1,234.5 \| Nov 19, 2026$/)
    await act(() => setPreferences({ user: 'pt-BR' }))
    expect(screen.getByRole('paragraph').textContent).toBe(`${ptBR.shell.logOut} | 1.234,5 | 19 de nov. de 2026`)
    expect(document.documentElement.lang).toBe('pt-BR')
    expect(document.documentElement.dir).toBe('ltr')
    expect(t('shell.logOut')).toBe(ptBR.shell.logOut)
    expect(localStorage.getItem('taskto.locale')).toBe('pt-BR')
  })

  it("follows the workspace's default without a pick", async () => {
    await act(() => setPreferences({ user: null, workspace: 'pt-BR' }))
    // jsdom's navigator.languages is en-US: English, which the app has, wins.
    expect(t('shell.logOut')).toBe('Log out')
    expect(localStorage.getItem('taskto.locale')).toBeNull()
  })
})

describe('browserLocale', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })
  const browser = (languages: string[]) => vi.stubGlobal('navigator', { languages, language: languages[0] })
  it("sends the first language in the server's form", () => {
    browser(['pt-br', 'en'])
    expect(browserLocale()).toBe('pt-BR')
    browser(['zh-Hant-TW'])
    expect(browserLocale()).toBe('zh-Hant-TW')
    browser(['de-DE-u-co-phonebk'])
    expect(browserLocale()).toBe('de-DE')
  })
  it('sends nothing for none or a tag the server would refuse', () => {
    browser([])
    expect(browserLocale()).toBe('')
    browser(['de-1996'])
    expect(browserLocale()).toBe('de')
  })
})

describe('calendar dates', () => {
  const zone = process.env.TZ
  afterEach(() => {
    if (zone === undefined) delete process.env.TZ
    else process.env.TZ = zone
  })

  it('shows the day that was written, in any year and time zone', () => {
    const { result } = renderHook(() => useFormat())
    // Years under 100 aren't 19xx.
    expect(result.current.calendarDate('0099-01-02')).toBe('Jan 2, 99')
    // Apia skipped 2011-12-30: a local-time date for it lands on the 31st.
    process.env.TZ = 'Pacific/Apia'
    expect(result.current.calendarDate('2011-12-30')).toBe('Dec 30, 2011')
    process.env.TZ = 'America/Los_Angeles'
    expect(result.current.calendarDate('2026-03-08')).toBe('Mar 8, 2026')
  })
})

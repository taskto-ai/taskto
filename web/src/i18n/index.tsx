// The web app in the user's language (docs/i18n.md). Every string people see comes from a
// locale file, locales/<BCP 47 tag>.json; English (en.json) is the reference, and the
// fallback for a key another file doesn't have yet. Adding a language is adding its file:
// it is found by its name, loaded when picked, and named in its own language by Intl.
//
// Messages are plain strings with {name} placeholders; a number fills one in in the
// locale's digits. A message that depends on a count has a key per plural category
// (`files_one`, `files_other`, and `_zero`, `_two`, `_few`, `_many` where the language has
// them), picked by Intl.PluralRules for params.count. <name>…</name> in a message wraps its
// text in the element params.name makes, for tx().
//
// The locale is the user's pick, else the first of the browser's languages the app has,
// else the workspace's default, else English. The page's lang and dir follow it.

import { Children, type ReactNode, useMemo, useSyncExternalStore } from 'react'
import en from './locales/en.json'

// --- Keys -------------------------------------------------------------------------------

type Messages = typeof en
type Plural = 'zero' | 'one' | 'two' | 'few' | 'many' | 'other'
type Leaves<T, P extends string = ''> = {
  [K in keyof T & string]: T[K] extends string ? `${P}${K}` : Leaves<T[K], `${P}${K}.`>
}[keyof T & string]
// A plural message is called by its key without the category: files for files_one and
// files_other, with params.count.
type Base<K extends string> = K extends `${infer B}_other`
  ? B
  : K extends `${string}_${Exclude<Plural, 'other'>}`
    ? never
    : K
export type Key = Base<Leaves<Messages>>

export type Param = string | number
export type Params = Record<string, Param>
export type RichParams = Record<string, Param | ReactNode | ((text: ReactNode) => ReactNode)>

// --- Locales ----------------------------------------------------------------------------

type Catalog = { [key: string]: string | Catalog }

export const DEFAULT_LOCALE = 'en'

// Every file but English, loaded when picked: each is a chunk of its own.
const files = import.meta.glob<Catalog>(['./locales/*.json', '!./locales/en.json'], { import: 'default' })
const loaders = new Map<string, () => Promise<Catalog>>(
  Object.entries(files).map(([path, load]) => [path.slice('./locales/'.length, -'.json'.length), load]),
)

// The locales the app has, English first, then by tag.
export const LOCALES: readonly string[] = [DEFAULT_LOCALE, ...[...loaders.keys()].sort()]

// A locale's name in its own language, as a menu shows it: "Português (Brasil)".
export function localeName(tag: string): string {
  try {
    const name = new Intl.DisplayNames([tag], { type: 'language' }).of(tag) ?? tag
    return name.charAt(0).toLocaleUpperCase(tag) + name.slice(1)
  } catch {
    return tag
  }
}

// Scripts written right to left: Arabic, Hebrew, Thaana, Syriac, N'Ko, Adlam, Hanifi
// Rohingya, Mandaic, Samaritan. By script rather than Intl.Locale's textInfo, which not
// every browser has.
const RTL_SCRIPTS = new Set(['Arab', 'Hebr', 'Thaa', 'Syrc', 'Nkoo', 'Adlm', 'Rohg', 'Mand', 'Samr'])

export function direction(tag: string): 'ltr' | 'rtl' {
  try {
    return RTL_SCRIPTS.has(new Intl.Locale(tag).maximize().script ?? '') ? 'rtl' : 'ltr'
  } catch {
    return 'ltr'
  }
}

// The first of `wanted` the app has: the tag itself, else another of the same language
// ("pt-PT" gets "pt-BR", "pt" too), one in the same script first ("zh-TW" gets "zh-Hant",
// not "zh-Hans"). Case doesn't matter; null if none matches.
export function match(wanted: readonly string[], available: readonly string[] = LOCALES): string | null {
  const lower = new Map(available.map((a) => [a.toLowerCase(), a]))
  const language = (tag: string) => tag.toLowerCase().split('-')[0]
  for (const w of wanted) {
    const exact = lower.get(w.toLowerCase())
    if (exact) return exact
    const same = available.filter((a) => language(a) === language(w))
    const written = script(w)
    const found = same.find((a) => script(a) === written) ?? same[0]
    if (found) return found
  }
  return null
}

// The script a tag is written in, its own or the likely one ("zh-TW" is Hant, "sr" is Cyrl);
// "" for a tag Intl can't read.
function script(tag: string): string {
  try {
    return new Intl.Locale(tag).maximize().script ?? ''
  } catch {
    return ''
  }
}

export type Preferences = { user: string | null; browser: readonly string[]; workspace: string | null }

// The locale to show: the user's pick, then the browser's, then the workspace's, then English.
export function resolve({ user, browser, workspace }: Preferences, available: readonly string[] = LOCALES): string {
  return (
    (user ? match([user], available) : null) ??
    match(browser, available) ??
    (workspace ? match([workspace], available) : null) ??
    DEFAULT_LOCALE
  )
}

// --- The current locale -----------------------------------------------------------------

type State = { locale: string; catalog: Catalog }

let state: State = { locale: DEFAULT_LOCALE, catalog: en }
const listeners = new Set<() => void>()
const prefs: Preferences = { user: null, browser: [], workspace: null }
const STORED = 'taskto.locale'

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

function browserLanguages(): readonly string[] {
  return typeof navigator === 'undefined' ? [] : (navigator.languages ?? [navigator.language])
}

// The server's form of a tag: a language, a script and a region, in canonical case. The same
// pattern as internal/auth/locale.go's and agent/src/language.ts's; CI checks they agree
// (web/scripts/i18n-check.mjs).
const SERVER_TAG = /^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$/

// The browser's first language in the form the server takes, or '' (none, or one it can't
// read): for the agent, which can speak a language the app has no file for.
export function browserLocale(): string {
  const first = browserLanguages()[0]
  if (!first) return ''
  try {
    const l = new Intl.Locale(first)
    const tag = [l.language, l.script, l.region].filter(Boolean).join('-')
    return SERVER_TAG.test(tag) ? tag : ''
  } catch {
    return ''
  }
}

// The user's pick as this browser last saw it, so a reload shows it before /api/me answers.
function storedChoice(): string | null {
  try {
    return localStorage.getItem(STORED)
  } catch {
    return null
  }
}

function storeChoice(tag: string | null) {
  try {
    if (tag) localStorage.setItem(STORED, tag)
    else localStorage.removeItem(STORED)
  } catch {
    // Not remembered: the browser's language until /api/me answers.
  }
}

// Each change is numbered: a file that loads after a later change was asked for is dropped.
let changes = 0

async function apply(locale: string) {
  const change = ++changes
  if (locale === state.locale) return
  let catalog: Catalog = en
  const load = loaders.get(locale)
  if (load) {
    try {
      catalog = await load()
    } catch {
      locale = DEFAULT_LOCALE // its chunk didn't load (offline, a new deploy): English for now
    }
  }
  if (change !== changes || locale === state.locale) return
  state = { locale, catalog }
  if (typeof document !== 'undefined') {
    document.documentElement.lang = locale
    document.documentElement.dir = direction(locale)
  }
  for (const listener of listeners) listener()
}

// Before the first render: the stored pick or the browser's language, loaded.
export function initLocale(): Promise<void> {
  prefs.user = storedChoice()
  prefs.browser = browserLanguages()
  return apply(resolve(prefs))
}

// Updates what the locale follows: the user's pick (null for none, so the browser's), and the
// workspace's default. Either left out stays as it was.
export function setPreferences(change: { user?: string | null; workspace?: string | null }): Promise<void> {
  if (change.user !== undefined) {
    prefs.user = change.user || null
    storeChoice(prefs.user)
  }
  if (change.workspace !== undefined) prefs.workspace = change.workspace || null
  prefs.browser = browserLanguages()
  return apply(resolve(prefs))
}

// The locale the user gets without a pick of their own.
export function automaticLocale(): string {
  return resolve({ ...prefs, user: null, browser: browserLanguages() })
}

export function useLocale(): string {
  return useSyncExternalStore(subscribe, () => state.locale)
}

// --- Messages ---------------------------------------------------------------------------

function lookup(catalog: Catalog, key: string): string | undefined {
  let node: string | Catalog | undefined = catalog
  for (const part of key.split('.')) {
    if (node === undefined || typeof node === 'string') return undefined
    node = node[part]
  }
  return typeof node === 'string' ? node : undefined
}

const pluralRules = new Map<string, Intl.PluralRules>()
const numberFormats = new Map<string, Intl.NumberFormat>()

function cached<T>(cache: Map<string, T>, locale: string, make: () => T): T {
  let value = cache.get(locale)
  if (!value) {
    value = make()
    cache.set(locale, value)
  }
  return value
}

// The message for key in s's locale, else in English, else the key itself (a bug the type
// check and CI catch first).
function message(s: State, key: string, params?: { count?: unknown }): string {
  const count = params?.count
  if (typeof count === 'number') {
    const category = cached(pluralRules, s.locale, () => new Intl.PluralRules(s.locale)).select(count)
    // An exact 0 may have a message of its own ("No files"), in any language.
    const forms = count === 0 ? ['zero', category, 'other'] : [category, 'other']
    for (const catalog of [s.catalog, en as Catalog]) {
      for (const form of forms) {
        const found = lookup(catalog, `${key}_${form}`)
        if (found !== undefined) return found
      }
    }
  }
  return lookup(s.catalog, key) ?? lookup(en, key) ?? key
}

const PLACEHOLDER = /\{(\w+)\}/g

function fill(s: State, text: string, params: Params | undefined): string {
  if (!params) return text
  return text.replace(PLACEHOLDER, (whole, name: string) => {
    const value = params[name]
    if (value === undefined) return whole
    return typeof value === 'number'
      ? cached(numberFormats, s.locale, () => new Intl.NumberFormat(s.locale)).format(value)
      : value
  })
}

// A message with its placeholders filled in, as a string.
function translate(s: State, key: Key, params?: Params): string {
  return fill(s, message(s, key, params), params)
}

const TAG = /<(\w+)>(.*?)<\/\1>|\{(\w+)\}/gs

// A message as React nodes: {name} takes any node, and <name>text</name> is wrapped by the
// function params.name.
function translateRich(s: State, key: Key, params: RichParams): ReactNode {
  const text = message(s, key, params as { count?: unknown })
  // The params that are text or numbers, for the placeholders inside a tag's text.
  const plain: Params = {}
  for (const [name, value] of Object.entries(params)) {
    if (typeof value === 'string' || typeof value === 'number') plain[name] = value
  }
  const out: ReactNode[] = []
  let last = 0
  for (const m of text.matchAll(TAG)) {
    if (m.index > last) out.push(text.slice(last, m.index))
    const [whole, tag, inner, name] = m
    const value = params[tag ?? name]
    if (tag !== undefined) {
      const filled = fill(s, inner, plain)
      out.push(typeof value === 'function' ? value(filled) : filled)
    } else if (value === undefined) {
      out.push(whole)
    } else if (typeof value === 'number' || typeof value === 'string') {
      out.push(fill(s, whole, { [name]: value }))
    } else {
      out.push(value as ReactNode)
    }
    last = m.index + whole.length
  }
  if (last < text.length) out.push(text.slice(last))
  return Children.toArray(out) // keyed by place: a message's parts never move
}

export type T = ((key: Key, params?: Params) => string) & {
  rich: (key: Key, params: RichParams) => ReactNode
  locale: string
}

function translator(s: State): T {
  const t = ((key: Key, params?: Params) => translate(s, key, params)) as T
  t.rich = (key, params) => translateRich(s, key, params)
  t.locale = s.locale
  return t
}

// A translator over a catalog of one's own: for tests.
export function translatorFor(locale: string, catalog: Catalog): T {
  return translator({ locale, catalog })
}

// For components: t(key, params), re-rendering when the locale changes.
export function useT(): T {
  const s = useSyncExternalStore(subscribe, () => state)
  return useMemo(() => translator(s), [s])
}

// Outside components (errorText): the current locale's message, not re-rendered on a change.
export function t(key: Key, params?: Params): string {
  return translate(state, key, params)
}

// Whether key has a message, in the current locale or English: for keys made at run time,
// such as an API error's code.
export function has(key: string): boolean {
  return lookup(state.catalog, key) !== undefined || lookup(en, key) !== undefined
}

// --- Formats ----------------------------------------------------------------------------

export type Format = ReturnType<typeof formats>

function formats(locale: string) {
  const dateTime = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' })
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium' })
  const time = new Intl.DateTimeFormat(locale, { timeStyle: 'short' })
  const number = new Intl.NumberFormat(locale)
  const relative = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' })
  const lists = {
    conjunction: new Intl.ListFormat(locale),
    disjunction: new Intl.ListFormat(locale, { type: 'disjunction' }),
  }
  const toDate = (d: Date | string | number) => (d instanceof Date ? d : new Date(d))
  return {
    locale,
    number: (n: number, options?: Intl.NumberFormatOptions) =>
      options ? new Intl.NumberFormat(locale, options).format(n) : number.format(n),
    date: (d: Date | string | number) => date.format(toDate(d)),
    // A calendar date written YYYY-MM-DD (a card's due date), as that day wherever it is read:
    // not a moment, so no time zone moves it.
    calendarDate: (ymd: string) => {
      const [y, m, d] = ymd.split('-').map(Number)
      return date.format(new Date(y, m - 1, d))
    },
    time: (d: Date | string | number) => time.format(toDate(d)),
    dateTime: (d: Date | string | number) => dateTime.format(toDate(d)),
    // "5 minutes ago", "yesterday", "in 2 hours": the largest unit that fits.
    relative: (d: Date | string | number, now: number = Date.now()) => {
      const seconds = (toDate(d).getTime() - now) / 1000
      for (const [unit, size] of RELATIVE_UNITS) {
        if (Math.abs(seconds) >= size || unit === 'second') {
          return relative.format(Math.round(seconds / size), unit)
        }
      }
      return relative.format(0, 'second')
    },
    // A size in bytes: "512 B", "1.5 MB".
    bytes: (n: number) => {
      const [unit, size] = BYTE_UNITS.find(([, size]) => n >= size) ?? BYTE_UNITS[BYTE_UNITS.length - 1]
      return new Intl.NumberFormat(locale, {
        style: 'unit',
        unit,
        unitDisplay: 'short',
        maximumFractionDigits: unit === 'byte' ? 0 : 1,
      }).format(n / size)
    },
    list: (items: readonly string[], type: 'conjunction' | 'disjunction' = 'conjunction') => lists[type].format(items),
  }
}

const RELATIVE_UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 365 * 86400],
  ['month', 30 * 86400],
  ['week', 7 * 86400],
  ['day', 86400],
  ['hour', 3600],
  ['minute', 60],
  ['second', 1],
]

const BYTE_UNITS: [string, number][] = [
  ['gigabyte', 1e9],
  ['megabyte', 1e6],
  ['kilobyte', 1e3],
  ['byte', 1],
]

const formatCache = new Map<string, Format>()

// The formats of the current locale, outside a hook: for helpers called while rendering (a
// component that renders text also calls useT(), so it re-renders on a change).
export function format(): Format {
  return cached(formatCache, state.locale, () => formats(state.locale))
}

// Dates, times, numbers, sizes and lists in the current locale.
export function useFormat(): Format {
  const locale = useLocale()
  return useMemo(() => formats(locale), [locale])
}

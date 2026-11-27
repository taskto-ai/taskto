import { Children, type ReactNode, useMemo, useSyncExternalStore } from 'react'
import en from './locales/en.json'

type Messages = typeof en
type Plural = 'zero' | 'one' | 'two' | 'few' | 'many' | 'other'
type Leaves<T, P extends string = ''> = {
  [K in keyof T & string]: T[K] extends string ? `${P}${K}` : Leaves<T[K], `${P}${K}.`>
}[keyof T & string]
type Base<K extends string> = K extends `${infer B}_other`
  ? B
  : K extends `${string}_${Exclude<Plural, 'other'>}`
    ? never
    : K
export type Key = Base<Leaves<Messages>>

export type Param = string | number
export type Params = Record<string, Param>
export type RichParams = Record<string, Param | ReactNode | ((text: ReactNode) => ReactNode)>

type Catalog = { [key: string]: string | Catalog }

export const DEFAULT_LOCALE = 'en'

const files = import.meta.glob<Catalog>(['./locales/*.json', '!./locales/en.json'], { import: 'default' })
const loaders = new Map<string, () => Promise<Catalog>>(
  Object.entries(files).map(([path, load]) => [path.slice('./locales/'.length, -'.json'.length), load]),
)

export const LOCALES: readonly string[] = [DEFAULT_LOCALE, ...[...loaders.keys()].sort()]

export function localeName(tag: string): string {
  try {
    const name = new Intl.DisplayNames([tag], { type: 'language' }).of(tag) ?? tag
    return name.charAt(0).toLocaleUpperCase(tag) + name.slice(1)
  } catch {
    return tag
  }
}

const RTL_SCRIPTS = new Set(['Arab', 'Hebr', 'Thaa', 'Syrc', 'Nkoo', 'Adlm', 'Rohg', 'Mand', 'Samr'])

export function direction(tag: string): 'ltr' | 'rtl' {
  try {
    return RTL_SCRIPTS.has(new Intl.Locale(tag).maximize().script ?? '') ? 'rtl' : 'ltr'
  } catch {
    return 'ltr'
  }
}

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

function script(tag: string): string {
  try {
    return new Intl.Locale(tag).maximize().script ?? ''
  } catch {
    return ''
  }
}

export type Preferences = { user: string | null; browser: readonly string[]; workspace: string | null }

export function resolve({ user, browser, workspace }: Preferences, available: readonly string[] = LOCALES): string {
  return (
    (user ? match([user], available) : null) ??
    match(browser, available) ??
    (workspace ? match([workspace], available) : null) ??
    DEFAULT_LOCALE
  )
}

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

const SERVER_TAG = /^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$/

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
  } catch {}
}

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
      locale = DEFAULT_LOCALE
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

export function initLocale(): Promise<void> {
  prefs.user = storedChoice()
  prefs.browser = browserLanguages()
  return apply(resolve(prefs))
}

export function setPreferences(change: { user?: string | null; workspace?: string | null }): Promise<void> {
  if (change.user !== undefined) {
    prefs.user = change.user || null
    storeChoice(prefs.user)
  }
  if (change.workspace !== undefined) prefs.workspace = change.workspace || null
  prefs.browser = browserLanguages()
  return apply(resolve(prefs))
}

export function automaticLocale(): string {
  return resolve({ ...prefs, user: null, browser: browserLanguages() })
}

export function useLocale(): string {
  return useSyncExternalStore(subscribe, () => state.locale)
}

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

function message(s: State, key: string, params?: { count?: unknown }): string {
  const count = params?.count
  if (typeof count === 'number') {
    const category = cached(pluralRules, s.locale, () => new Intl.PluralRules(s.locale)).select(count)
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

function translate(s: State, key: Key, params?: Params): string {
  return fill(s, message(s, key, params), params)
}

const TAG = /<(\w+)>(.*?)<\/\1>|\{(\w+)\}/gs

function translateRich(s: State, key: Key, params: RichParams): ReactNode {
  const text = message(s, key, params as { count?: unknown })
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
  return Children.toArray(out)
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

export function translatorFor(locale: string, catalog: Catalog): T {
  return translator({ locale, catalog })
}

export function useT(): T {
  const s = useSyncExternalStore(subscribe, () => state)
  return useMemo(() => translator(s), [s])
}

export function t(key: Key, params?: Params): string {
  return translate(state, key, params)
}

export function has(key: string): boolean {
  return lookup(state.catalog, key) !== undefined || lookup(en, key) !== undefined
}

export type Format = ReturnType<typeof formats>

function formats(locale: string) {
  const dateTime = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' })
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium' })
  const calendarDate = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeZone: 'UTC' })
  const time = new Intl.DateTimeFormat(locale, { timeStyle: 'short' })
  const number = new Intl.NumberFormat(locale)
  const relative = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' })
  const lists = {
    conjunction: new Intl.ListFormat(locale),
    disjunction: new Intl.ListFormat(locale, { type: 'disjunction' }),
    unit: new Intl.ListFormat(locale, { type: 'unit', style: 'short' }),
  }
  const toDate = (d: Date | string | number) => (d instanceof Date ? d : new Date(d))
  return {
    locale,
    number: (n: number, options?: Intl.NumberFormatOptions) =>
      options ? new Intl.NumberFormat(locale, options).format(n) : number.format(n),
    date: (d: Date | string | number) => date.format(toDate(d)),
    calendarDate: (ymd: string) => {
      const [y, m, d] = ymd.split('-').map(Number)
      const day = new Date(0)
      day.setUTCFullYear(y, m - 1, d)
      return calendarDate.format(day)
    },
    time: (d: Date | string | number) => time.format(toDate(d)),
    dateTime: (d: Date | string | number) => dateTime.format(toDate(d)),
    relative: (d: Date | string | number, now: number = Date.now()) => {
      const seconds = (toDate(d).getTime() - now) / 1000
      for (const [unit, size] of RELATIVE_UNITS) {
        if (Math.abs(seconds) >= size || unit === 'second') {
          return relative.format(Math.round(seconds / size), unit)
        }
      }
      return relative.format(0, 'second')
    },
    bytes: (n: number) => {
      const [unit, size] = BYTE_UNITS.find(([, size]) => n >= size) ?? BYTE_UNITS[BYTE_UNITS.length - 1]
      const parts = new Intl.NumberFormat(locale, {
        style: 'unit',
        unit,
        unitDisplay: 'short',
        maximumFractionDigits: unit === 'byte' ? 0 : 1,
      }).formatToParts(n / size)
      return parts.map((p) => (unit === 'byte' && p.type === 'unit' ? 'B' : p.value)).join('')
    },
    list: (items: readonly string[], type: 'conjunction' | 'disjunction' | 'unit' = 'conjunction') =>
      lists[type].format(items),
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

export function format(): Format {
  return cached(formatCache, state.locale, () => formats(state.locale))
}

export function useFormat(): Format {
  const locale = useLocale()
  return useMemo(() => formats(locale), [locale])
}

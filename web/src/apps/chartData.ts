export const MAX_SERIES_DRAWN = 6
export const MAX_SERIES_LISTED = 24
export const MAX_POINTS = 120
export const MAX_MAGNITUDE = 1e15
export const MAX_TEXT = 80

export type ChartPoint = { key: string; label: string }
export type ChartLine = { key: string; label: string; values: (number | null)[] }
export type ChartData = { x: ChartPoint[]; series: ChartLine[] }
export type Layer = { from: number; to: number; has: boolean }
export type Scale = { min: number; max: number; ticks: number[] }

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

function own(o: Record<string, unknown>, key: string): unknown {
  return Object.hasOwn(o, key) ? o[key] : undefined
}

export function cut(text: string, max = MAX_TEXT): string {
  const chars = Array.from(text)
  return chars.length > max ? `${chars.slice(0, max).join('')}…` : text
}

function textOf(v: unknown): string {
  return typeof v === 'string' ? cut(v) : ''
}

export function readValue(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) && Math.abs(v) <= MAX_MAGNITUDE ? v : null
}

export function readChart(raw: unknown): ChartData | null {
  if (!isRecord(raw)) return null
  const xs = own(raw, 'x')
  const lines = own(raw, 'series')
  if (!Array.isArray(xs) || !Array.isArray(lines) || xs.length === 0) return null
  const x = xs.slice(0, MAX_POINTS).map((p): ChartPoint => {
    const point = isRecord(p) ? p : {}
    const key = textOf(own(point, 'key'))
    const label = textOf(own(point, 'label'))
    return { key, label: label || key }
  })
  const series: ChartLine[] = []
  for (const line of lines) {
    if (series.length === MAX_SERIES_LISTED) break
    if (!isRecord(line)) continue
    const values = own(line, 'values')
    if (!Array.isArray(values)) continue
    const key = textOf(own(line, 'key'))
    series.push({
      key,
      label: textOf(own(line, 'label')) || key,
      values: x.map((_, i) => readValue(values[i])),
    })
  }
  return series.length === 0 ? null : { x, series }
}

const MANTISSAS = [1, 2, 2.5, 5, 10]

export function niceMax(n: number): number {
  const wanted = Math.max(n, 1e-9)
  const base = 10 ** Math.floor(Math.log10(wanted))
  const m = wanted / base
  const found = MANTISSAS.find((s) => m <= s * (1 + 1e-9)) ?? 10
  return Number((found * base).toPrecision(12))
}

function intervalsOf(top: number): number {
  const m = top / 10 ** Math.floor(Math.log10(top))
  return Math.abs(m - 2.5) < 1e-9 || Math.abs(m - 5) < 1e-9 ? 5 : 4
}

export function chartScale(values: readonly number[]): Scale {
  let hi = 0
  let lo = 0
  for (const v of values) {
    if (v > hi) hi = v
    if (v < lo) lo = v
  }
  const extent = Math.max(hi, -lo)
  const top = niceMax(extent > 0 ? extent : 1)
  const intervals = intervalsOf(top)
  const step = top / intervals
  const round = (n: number) => Number(n.toPrecision(12))
  let min = round(Math.floor(lo / step + 1e-9) * step)
  if (min > lo) min = round(min - step)
  let max = round(Math.ceil(hi / step - 1e-9) * step)
  if (max < hi) max = round(max + step)
  if (max <= min) max = round(min + step * intervals)
  const ticks: number[] = []
  for (let i = Math.round(min / step); i <= Math.round(max / step); i++) ticks.push(round(i * step))
  return { min, max, ticks }
}

export function stackLayers(series: readonly ChartLine[]): Layer[][] {
  const totals: number[] = []
  return series.map((line) =>
    line.values.map((v, i) => {
      const from = totals[i] ?? 0
      const to = from + (v ?? 0)
      totals[i] = to
      return { from, to, has: v !== null }
    }),
  )
}

export function plotted(series: readonly ChartLine[], stacked: boolean): number[] {
  if (!stacked) return series.flatMap((line) => line.values.filter((v): v is number => v !== null))
  return stackLayers(series).flatMap((layers) => layers.flatMap((l) => (l.has ? [l.from, l.to] : [])))
}

export type Run = { start: number; end: number }

export function runs(values: readonly (number | null)[]): Run[] {
  const out: Run[] = []
  let start = -1
  values.forEach((v, i) => {
    if (v !== null && start < 0) start = i
    if (v === null && start >= 0) {
      out.push({ start, end: i - 1 })
      start = -1
    }
  })
  if (start >= 0) out.push({ start, end: values.length - 1 })
  return out
}

export const TICK_SIZE = 12

const EM_WIDTHS: [string, number][] = [
  [" !'.,:;Iijl", 0.3],
  ['()[]{}/\\|ftr`', 0.4],
  ['"^_?s', 0.53],
  ['abcdeghknopquvxyz', 0.62],
  ['0123456789#$&*+-<=>~', 0.65],
  ['BEFJKLPRSTXYZ', 0.69],
  ['ACDGHNOQUV', 0.77],
  ['w', 0.82],
  ['m', 0.9],
  ['M', 0.91],
  ['%@ÆŒæœ', 0.99],
  ['W', 1.02],
  ['…', 1.1],
]

const EM = new Map(EM_WIDTHS.flatMap(([chars, em]) => Array.from(chars, (c): [string, number] => [c, em])))

function emOf(ch: string): number {
  const known = EM.get(ch)
  if (known !== undefined) return known
  const code = ch.codePointAt(0) ?? 0
  if (code >= 0x300 && code <= 0x36f) return 0
  if (/\p{Nd}/u.test(ch)) return 0.7
  if (code <= 0x24f) return 0.77
  if (code <= 0x52f) return 1
  return 1.3
}

export function labelWidth(text: string): number {
  return Array.from(text).reduce((sum, ch) => sum + emOf(ch), 0) * TICK_SIZE
}

export type Anchor = 'start' | 'middle' | 'end'

export function anchorOf(i: number, count: number): Anchor {
  if (count === 1) return 'middle'
  return i === 0 ? 'start' : i === count - 1 ? 'end' : 'middle'
}

export function axisPicks(count: number, widths: readonly number[], band = false, width = 272, gap = 10): number[] {
  const extent = (i: number): [number, number] => {
    const x = band ? ((i + 0.5) * width) / count : count === 1 ? width / 2 : (i * width) / (count - 1)
    const w = widths[i] ?? 0
    const anchor = anchorOf(i, count)
    if (anchor === 'start') return [x, x + w]
    if (anchor === 'end') return [x - w, x]
    return [x - w / 2, x + w / 2]
  }
  const fits = (picks: number[]) => {
    const gaps = picks.slice(1).map((p, k) => p - picks[k])
    if (gaps.length > 1 && Math.min(...gaps) * 4 < Math.max(...gaps) * 3) return false
    return picks.every((p, k) => k === 0 || extent(p)[0] - extent(picks[k - 1])[1] >= gap)
  }
  for (let wanted = count > 6 ? 3 : count; wanted > 1; wanted--) {
    const picks = Array.from({ length: wanted }, (_, i) => Math.round((i * (count - 1)) / (wanted - 1)))
    if (fits(picks)) return picks
  }
  return [0]
}

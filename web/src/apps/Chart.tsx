import { createComponentImplementation } from '@a2ui/react/v0_9'
import { CommonSchemas } from '@a2ui/web_core/v0_9'
import { type ReactNode, useMemo } from 'react'
import { z } from 'zod'
import { type Format, type T, useFormat, useT } from '../i18n'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import {
  axisPicks,
  type ChartData,
  type ChartLine,
  type ChartPoint,
  chartScale,
  cut,
  MAX_SERIES_DRAWN,
  plotted,
  type Run,
  readChart,
  runs,
  stackLayers,
} from './chartData'
import './Chart.css'

export const ChartApi = {
  name: 'Chart',
  schema: z
    .object({
      data: CommonSchemas.DynamicValue,
      variant: z.enum(['line', 'bar', 'area']).optional(),
      label: CommonSchemas.DynamicString,
      stacked: z.boolean().optional(),
      unit: CommonSchemas.DynamicString.optional(),
      accessibility: CommonSchemas.AccessibilityAttributes.optional(),
      weight: z.number().optional(),
    })
    .strict(),
}

const SERIES_CLASS = ['ui-chart-s1', 'ui-chart-s2', 'ui-chart-s3', 'ui-chart-s4', 'ui-chart-s5', 'ui-chart-s6']

const WIDTH = 320
const HEIGHT = 180
const LEFT = 40
const RIGHT = 8
const TOP = 18
const BOTTOM = 24
const PLOT_WIDTH = WIDTH - LEFT - RIGHT
const PLOT_HEIGHT = HEIGHT - TOP - BOTTOM
const AXIS_LABEL = 14

const DAY = /^(\d{4})-(\d{2})-(\d{2})$/
const WEEK = /^(\d{4})-W(\d{2})$/
const MONTH = /^(\d{4})-(\d{2})$/

function realDay(y: number, m: number, d: number): boolean {
  const day = new Date(0)
  day.setUTCFullYear(y, m - 1, d)
  return day.getUTCFullYear() === y && day.getUTCMonth() === m - 1 && day.getUTCDate() === d
}

export function pointText(p: ChartPoint, t: T, f: Format): string {
  if (p.label !== p.key) return p.label
  if (p.key === 'true') return t('apps.table.yes')
  if (p.key === 'false') return t('apps.table.no')
  if (p.key === 'other') return t('apps.chart.other')
  const day = DAY.exec(p.key)
  if (day && realDay(Number(day[1]), Number(day[2]), Number(day[3]))) return f.calendarDate(p.key)
  const week = WEEK.exec(p.key)
  if (week && Number(week[2]) >= 1 && Number(week[2]) <= 53)
    return t('apps.chart.week', { week: f.number(Number(week[2]), { useGrouping: false }), year: week[1] })
  const month = MONTH.exec(p.key)
  if (month && Number(month[2]) >= 1 && Number(month[2]) <= 12) return f.calendarMonth(p.key)
  return p.label
}

function lineText(line: ChartLine, t: T, f: Format): string {
  return line.label === '' ? t('apps.chart.value') : pointText(line, t, f)
}

const DIGITS = { maximumFractionDigits: 10 }

function cell(value: number | null, unit: string, f: Format): string {
  if (value === null) return '-'
  return unit === '' ? f.number(value, DIGITS) : `${f.number(value, DIGITS)} ${unit}`
}

const num = (n: number) => String(Math.round(n * 100) / 100)

function yOf(value: number, min: number, max: number): number {
  return TOP + (PLOT_HEIGHT * (max - value)) / (max - min)
}

function xOf(i: number, count: number, band: boolean): number {
  if (band) return LEFT + ((i + 0.5) * PLOT_WIDTH) / count
  return LEFT + (count === 1 ? PLOT_WIDTH / 2 : (i * PLOT_WIDTH) / (count - 1))
}

type Plot = { variant: 'line' | 'bar' | 'area'; stacked: boolean; min: number; max: number; count: number }

function marks(data: ChartData, p: Plot): ReactNode[] {
  const drawn = data.series.slice(0, MAX_SERIES_DRAWN)
  const stack = p.stacked && p.variant !== 'line'
  const layers = stack ? stackLayers(drawn) : null
  const y = (v: number) => yOf(v, p.min, p.max)
  const out: ReactNode[] = []
  if (p.variant === 'bar') {
    const band = PLOT_WIDTH / p.count
    const group = band * 0.8
    const width = stack ? group : group / drawn.length
    drawn.forEach((line, s) => {
      line.values.forEach((v, i) => {
        if (v === null) return
        const layer = layers?.[s][i]
        const from = layer ? layer.from : 0
        const to = layer ? layer.to : v
        const left = LEFT + i * band + (band - group) / 2 + (stack ? 0 : s * width)
        out.push(
          <rect
            key={`mark-${out.length}`}
            className={`ui-chart-mark ${SERIES_CLASS[s]}`}
            x={num(left)}
            y={num(Math.min(y(from), y(to)))}
            width={num(width)}
            height={num(Math.abs(y(from) - y(to)))}
          />,
        )
      })
    })
    return out
  }
  drawn.forEach((line, s) => {
    const cls = SERIES_CLASS[s]
    const at = (i: number, v: number) => `${num(xOf(i, p.count, false))} ${num(y(v))}`
    if (layers) {
      const ys = layers[s]
      const stretches = runs(ys.map((l) => (l.has ? l.to : null)))
      const top = (r: Run) =>
        ys
          .slice(r.start, r.end + 1)
          .map((l, k) => `${k === 0 ? 'M' : 'L'}${at(r.start + k, l.to)}`)
          .join('')
      if (p.variant === 'area') {
        const fill = stretches
          .filter((r) => r.end > r.start)
          .map((r) => {
            const back = ys
              .slice(r.start, r.end + 1)
              .map((l, k) => `L${at(r.start + k, l.from)}`)
              .reverse()
              .join('')
            return `${top(r)}${back}Z`
          })
          .join('')
        if (fill) out.push(<path key={`mark-${out.length}`} className={`ui-chart-fill ${cls}`} d={fill} />)
      }
      out.push(<path key={`mark-${out.length}`} className={`ui-chart-line ${cls}`} d={stretches.map(top).join('')} />)
      for (const r of stretches) {
        if (r.start !== r.end) continue
        out.push(
          <circle
            key={`mark-${out.length}`}
            className={`ui-chart-dot ${cls}`}
            cx={num(xOf(r.start, p.count, false))}
            cy={num(y(ys[r.start].to))}
            r="2.5"
          />,
        )
      }
      return
    }
    const parts = runs(line.values)
    const edge = (r: { start: number; end: number }) =>
      line.values
        .slice(r.start, r.end + 1)
        .map((v, k) => `${k === 0 ? 'M' : 'L'}${at(r.start + k, v as number)}`)
        .join('')
    const outline = parts.map(edge).join('')
    if (p.variant === 'area') {
      const floor = num(y(Math.min(Math.max(0, p.min), p.max)))
      const fill = parts
        .filter((r) => r.end > r.start)
        .map(
          (r) =>
            `${edge(r)}L${num(xOf(r.end, p.count, false))} ${floor}L${num(xOf(r.start, p.count, false))} ${floor}Z`,
        )
        .join('')
      if (fill) out.push(<path key={`mark-${out.length}`} className={`ui-chart-fill ${cls}`} d={fill} />)
    }
    out.push(<path key={`mark-${out.length}`} className={`ui-chart-line ${cls}`} d={outline} />)
    for (const r of parts) {
      if (r.start !== r.end) continue
      out.push(
        <circle
          key={`mark-${out.length}`}
          className={`ui-chart-dot ${cls}`}
          cx={num(xOf(r.start, p.count, false))}
          cy={num(y(line.values[r.start] as number))}
          r="2.5"
        />,
      )
    }
  })
  return out
}

function ChartDrawing({
  data,
  variant,
  stacked,
  unit,
  label,
}: {
  data: ChartData
  variant: 'line' | 'bar' | 'area'
  stacked: boolean
  unit: string
  label: string
}) {
  const t = useT()
  const f = useFormat()
  const drawn = useMemo(() => data.series.slice(0, MAX_SERIES_DRAWN), [data])
  const scale = useMemo(() => chartScale(plotted(drawn, stacked && variant !== 'line')), [drawn, stacked, variant])
  const columns = data.series.map((line, s) => ({ id: `column-${s}`, line }))
  const rows = data.x.map((point, i) => ({ id: `row-${i}`, point, i }))
  const count = data.x.length
  const band = variant === 'bar'
  const texts = data.x.map((p) => cut(pointText(p, t, f), AXIS_LABEL))
  const longest = Math.max(...texts.map((s) => Array.from(s).length))
  const picks = axisPicks(count, longest, band)
  const plot: Plot = { variant, stacked, min: scale.min, max: scale.max, count }
  return (
    <>
      <svg
        className="ui-chart-svg"
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
        role="img"
        aria-label={label}
        focusable="false"
        direction="ltr"
      >
        {unit !== '' && (
          <text className="ui-chart-unit" x="0" y="10">
            {unit}
          </text>
        )}
        {scale.ticks.map((tick) => {
          const y = yOf(tick, scale.min, scale.max)
          return (
            <g key={tick}>
              <line
                className={tick === 0 ? 'ui-chart-axis' : 'ui-chart-grid'}
                x1={LEFT}
                x2={WIDTH - RIGHT}
                y1={num(y)}
                y2={num(y)}
              />
              <text className="ui-chart-tick" x={LEFT - 6} y={num(y + 4)} textAnchor="end">
                {f.number(tick, DIGITS)}
              </text>
            </g>
          )
        })}
        {marks(data, plot)}
        {picks.map((i) => (
          <text
            key={i}
            className="ui-chart-tick"
            x={num(xOf(i, count, band))}
            y={HEIGHT - 6}
            textAnchor={count === 1 ? 'middle' : i === 0 ? 'start' : i === count - 1 ? 'end' : 'middle'}
          >
            {texts[i]}
          </text>
        ))}
      </svg>
      {drawn.length > 1 && (
        <ul className="ui-chart-legend">
          {columns.slice(0, drawn.length).map(({ id, line }, s) => (
            <li key={id} className="ui-chart-legend-item">
              <span className={`ui-chart-swatch ${SERIES_CLASS[s]}`} aria-hidden="true" />
              {lineText(line, t, f)}
            </li>
          ))}
        </ul>
      )}
      <VisuallyHidden>
        <table>
          <caption>{t('apps.chart.table', { label })}</caption>
          <thead>
            <tr>
              <th scope="col">{t('apps.chart.group')}</th>
              {columns.map(({ id, line }) => (
                <th key={id} scope="col">
                  {lineText(line, t, f)}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map(({ id, point, i }) => (
              <tr key={id}>
                <th scope="row">{pointText(point, t, f)}</th>
                {columns.map(({ id: column, line }) => (
                  <td key={column}>{cell(line.values[i], unit, f)}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </VisuallyHidden>
    </>
  )
}

export function ChartView({
  data,
  variant,
  stacked,
  unit,
  label,
}: {
  data: unknown
  variant: 'line' | 'bar' | 'area' | undefined
  stacked: boolean | undefined
  unit: unknown
  label: unknown
}) {
  const t = useT()
  const read = useMemo(() => readChart(data), [data])
  const name = typeof label === 'string' ? label : ''
  return (
    <figure className="ui-chart">
      <figcaption className="ui-chart-caption">{name}</figcaption>
      {read ? (
        <ChartDrawing
          data={read}
          variant={variant ?? 'line'}
          stacked={stacked === true}
          unit={typeof unit === 'string' ? cut(unit, 16) : ''}
          label={name}
        />
      ) : (
        <p className="ui-chart-empty">{t('apps.chart.empty')}</p>
      )}
    </figure>
  )
}

export const ChartComponent = createComponentImplementation(ChartApi, ({ props }) => (
  <ChartView data={props.data} variant={props.variant} stacked={props.stacked} unit={props.unit} label={props.label} />
))

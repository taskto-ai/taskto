// @vitest-environment jsdom

import { readFileSync } from 'node:fs'
import { cleanup, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'
import { setPreferences } from '../i18n'
import { AppView } from './AppView'
import { ChartApi } from './Chart'
import { APPS_CATALOG_ID, DEFINITION_VERSION } from './catalog'
import {
  axisPicks,
  type ChartLine,
  chartScale,
  labelWidth,
  MAX_POINTS,
  MAX_SERIES_LISTED,
  niceMax,
  plotted,
  readChart,
  runs,
  stackLayers,
  TICK_SIZE,
} from './chartData'
import { INTER_ADVANCE_EM, INTER_STRING_PX } from './chartInterWidths'

afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  await setPreferences({ user: null })
})

const stages = {
  x: [
    { key: 'lead', label: 'Lead' },
    { key: 'won', label: 'Won' },
    { key: 'lost', label: 'Lost' },
  ],
  series: [{ key: '', label: '', values: [3, 1234.5, 0] }],
}

const split = {
  x: [
    { key: '2026-09', label: '2026-09' },
    { key: '2026-10', label: '2026-10' },
    { key: '2026-11', label: '2026-11' },
  ],
  series: [
    { key: 'paid', label: 'Paid', values: [10, 20, 30] },
    { key: 'open', label: 'Open', values: [5, null, 15] },
  ],
}

function surface(chart: Record<string, unknown>, model: Record<string, unknown>) {
  return [
    { version: 'v0.9', createSurface: { surfaceId: 'app-1', catalogId: APPS_CATALOG_ID } },
    {
      version: 'v0.9',
      updateComponents: {
        surfaceId: 'app-1',
        components: [
          { id: 'root', component: 'Chart', data: { path: '/by_stage' }, label: 'Deals per stage', ...chart },
        ],
      },
    },
    { version: 'v0.9', updateDataModel: { surfaceId: 'app-1', path: '/', value: { items: [], draft: {}, ...model } } },
  ]
}

async function drawn(chart: Record<string, unknown>, model: Record<string, unknown>) {
  const messages = surface(chart, model)
  const view = render(
    <AppView
      surfaceId="app-1"
      label="Pipeline"
      release={{ definition_version: DEFINITION_VERSION, requires: ['component.Chart'] }}
      transport={{ load: async () => structuredClone(messages), act: async () => [] }}
    />,
  )
  await screen.findByRole('figure')
  return view
}

const svg = () => screen.getByRole('img', { hidden: false }) as unknown as SVGSVGElement
const count = (selector: string) => document.querySelectorAll(selector).length
const tableRows = () =>
  [...screen.getByRole('table', { hidden: true }).querySelectorAll('tr')].map((tr) =>
    [...tr.children].map((c) => c.textContent),
  )

describe('drawing', () => {
  test('a figure holds the caption, the labelled image and a table with the numbers', async () => {
    await drawn({ variant: 'bar' }, { by_stage: stages })
    const figure = screen.getByRole('figure')
    expect(within(figure).getByText('Deals per stage').tagName).toBe('FIGCAPTION')
    expect(svg().getAttribute('aria-label')).toBe('Deals per stage')
    expect(tableRows()).toEqual([
      ['Group', 'Value'],
      ['Lead', '3'],
      ['Won', '1,234.5'],
      ['Lost', '0'],
    ])
    expect(screen.getByRole('table', { hidden: true }).querySelector('caption')?.textContent).toBe(
      'Data of Deals per stage',
    )
  })

  test('small values keep their digits in the table and on the scale', async () => {
    const x = [
      { key: 'a', label: 'A' },
      { key: 'b', label: 'B' },
      { key: 'c', label: 'C' },
    ]
    await drawn(
      { variant: 'bar' },
      { by_stage: { x, series: [{ key: '', label: '', values: [0.0001, 0.0002, 0.0003] }] } },
    )
    expect(tableRows().slice(1)).toEqual([
      ['A', '0.0001'],
      ['B', '0.0002'],
      ['C', '0.0003'],
    ])
    const ticks = [...document.querySelectorAll('svg text.ui-chart-tick[text-anchor="end"]')].map((t) => t.textContent)
    expect(ticks.slice(0, 4)).toEqual(['0', '0.0001', '0.0002', '0.0003'])
  })

  test('a bar chart has one rect for each value that is there, and none for a null', async () => {
    await drawn({ variant: 'bar' }, { by_stage: split })
    expect(count('svg rect')).toBe(5)
    expect(count('svg path')).toBe(0)
    expect(tableRows()[2]).toEqual(['Oct 2026', '20', '-'])
  })

  test('a line chart has one path for each series and a gap where a value is null', async () => {
    await drawn({ variant: 'line' }, { by_stage: split })
    expect(count('svg path.ui-chart-line')).toBe(2)
    expect(count('svg rect')).toBe(0)
    const paths = [...document.querySelectorAll('svg path.ui-chart-line')].map((p) => p.getAttribute('d') ?? '')
    expect(paths[0].match(/M/g)).toHaveLength(1)
    expect(paths[1].match(/M/g)).toHaveLength(2)
    expect(count('svg circle')).toBe(2)
  })

  test('an area chart fills each series under its line', async () => {
    await drawn({ variant: 'area' }, { by_stage: split })
    expect(count('svg path.ui-chart-fill')).toBe(1)
    expect(count('svg path.ui-chart-line')).toBe(2)
  })

  test('a stacked bar puts each value on top of the one before it', async () => {
    await drawn({ variant: 'bar', stacked: true }, { by_stage: split })
    const rects = [...document.querySelectorAll('svg rect')]
    const first = rects.filter((r) => r.classList.contains('ui-chart-s1'))
    const second = rects.filter((r) => r.classList.contains('ui-chart-s2'))
    expect(first).toHaveLength(3)
    expect(second).toHaveLength(2)
    const top = (r: Element) => Number(r.getAttribute('y'))
    const bottom = (r: Element) => Number(r.getAttribute('y')) + Number(r.getAttribute('height'))
    expect(bottom(second[0])).toBeCloseTo(top(first[0]), 1)
    expect(top(second[0])).toBeLessThan(top(first[0]))
  })

  test('a line is never stacked: stacked changes nothing about it', async () => {
    const lineDs = () => [...document.querySelectorAll('svg path.ui-chart-line')].map((p) => p.getAttribute('d'))
    const view = await drawn({ variant: 'line', stacked: true }, { by_stage: split })
    const stacked = lineDs()
    view.unmount()
    cleanup()
    await drawn({ variant: 'line' }, { by_stage: split })
    expect(stacked).toEqual(lineDs())
  })

  test('a stacked area closes each layer over the one below', async () => {
    await drawn({ variant: 'area', stacked: true }, { by_stage: split })
    expect(count('svg path.ui-chart-fill')).toBe(1)
    expect(document.querySelector('svg path.ui-chart-fill.ui-chart-s1')?.getAttribute('d')).toMatch(/Z$/)
  })

  test('a stacked area does not connect across a missing value', async () => {
    const x = Array.from({ length: 5 }, (_, i) => ({ key: `k${i}`, label: `P${i}` }))
    const gap = {
      x,
      series: [
        { key: 'a', label: 'A', values: [10, 20, 30, 40, 50] },
        { key: 'b', label: 'B', values: [5, 6, null, 7, 8] },
      ],
    }
    await drawn({ variant: 'area', stacked: true }, { by_stage: gap })
    const second = (kind: string) => document.querySelector(`svg path.${kind}.ui-chart-s2`)?.getAttribute('d') ?? ''
    expect(second('ui-chart-line').match(/M/g)).toHaveLength(2)
    expect(second('ui-chart-fill').match(/M/g)).toHaveLength(2)
    expect(second('ui-chart-fill').match(/Z/g)).toHaveLength(2)
    expect(document.querySelector('svg path.ui-chart-fill.ui-chart-s1')?.getAttribute('d')?.match(/M/g)).toHaveLength(1)
    expect(count('svg circle')).toBe(0)
  })

  test('a stacked area keeps a lone value as a dot', async () => {
    await drawn({ variant: 'area', stacked: true }, { by_stage: split })
    expect(count('svg path.ui-chart-fill.ui-chart-s2')).toBe(0)
    expect(count('svg circle.ui-chart-s2')).toBe(2)
  })

  test('a legend names every drawn series only when there are several', async () => {
    const view = await drawn({}, { by_stage: split })
    expect(
      within(screen.getByRole('list'))
        .getAllByRole('listitem')
        .map((i) => i.textContent),
    ).toEqual(['Paid', 'Open'])
    view.unmount()
    cleanup()
    await drawn({}, { by_stage: stages })
    expect(screen.queryByRole('list')).toBeNull()
  })

  test('the unit follows the numbers in the table and sits on the axis', async () => {
    await drawn({ variant: 'bar', unit: 'kg' }, { by_stage: stages })
    expect(tableRows()[2]).toEqual(['Won', '1,234.5 kg'])
    expect(svg().querySelector('text.ui-chart-unit')?.textContent).toBe('kg')
  })

  test('the first six series are drawn and the table lists them all', async () => {
    const series: ChartLine[] = Array.from({ length: 8 }, (_, i) => ({
      key: `s${i}`,
      label: `Series ${i}`,
      values: [i, i + 1, i + 2],
    }))
    await drawn({ variant: 'line' }, { by_stage: { x: stages.x, series } })
    expect(count('svg path.ui-chart-line')).toBe(6)
    expect(screen.getAllByRole('listitem')).toHaveLength(6)
    expect(tableRows()[0]).toHaveLength(9)
  })

  test('at most one hundred and twenty points are kept, and only first, middle and last are named on the axis', async () => {
    const x = Array.from({ length: 500 }, (_, i) => ({ key: `k${i}`, label: `Point ${i}` }))
    await drawn({ variant: 'bar' }, { by_stage: { x, series: [{ key: '', label: '', values: x.map((_, i) => i) }] } })
    expect(tableRows()).toHaveLength(MAX_POINTS + 1)
    expect(count('svg rect')).toBe(MAX_POINTS)
    expect([...document.querySelectorAll('svg text.ui-chart-tick')].map((t) => t.textContent)).toContain('Point 0')
    expect(count('svg text.ui-chart-tick[y="174"]')).toBe(3)
    expect(count('svg text.ui-chart-tick[y="174"][text-anchor="start"]')).toBe(1)
    expect(count('svg text.ui-chart-tick[y="174"][text-anchor="end"]')).toBe(1)
  })

  test('the plot keeps its left to right reading in a right to left page', async () => {
    await drawn({}, { by_stage: stages })
    expect(svg().getAttribute('direction')).toBe('ltr')
  })
})

describe('no data', () => {
  test.each([
    ['a missing value', {}],
    ['text', { by_stage: 'many' }],
    ['a list', { by_stage: [1, 2] }],
    ['no points', { by_stage: { x: [], series: [{ key: '', label: '', values: [] }] } }],
    ['no series', { by_stage: { x: stages.x, series: [] } }],
    ['series that are not lists', { by_stage: { x: stages.x, series: [{ values: 'no' }, 7, null] } }],
    ['points that are not a list', { by_stage: { x: 'a', series: stages.series } }],
  ])('%s say so, still with the caption', async (_, model) => {
    await drawn({}, model)
    expect(screen.getByText('No data yet')).toBeTruthy()
    expect(screen.getByText('Deals per stage').tagName).toBe('FIGCAPTION')
    expect(count('svg')).toBe(0)
    expect(count('table')).toBe(0)
  })
})

describe('what is drawn from other people’s numbers', () => {
  test('a value that is not a plain number is a gap, and nothing drawn says NaN or Infinity', async () => {
    const values = [Number.POSITIVE_INFINITY, Number.NaN, '5', null, 1e300, -1e16, { n: 1 }, true, 4]
    const x = values.map((_, i) => ({ key: `k${i}`, label: `P${i}` }))
    await drawn({ variant: 'area' }, { by_stage: { x, series: [{ key: '', label: '', values }] } })
    expect(document.querySelector('figure')?.innerHTML).not.toMatch(/NaN|Infinity|undefined|null/)
    expect(
      tableRows()
        .slice(1)
        .map((r) => r[1]),
    ).toEqual(['-', '-', '-', '-', '-', '-', '-', '-', '4'])
    expect(count('svg circle')).toBe(1)
  })

  test('huge stacked values stay finite', async () => {
    const big = { key: '', label: '', values: [9e14, 9e14] }
    await drawn(
      { variant: 'area', stacked: true },
      { by_stage: { x: stages.x.slice(0, 2), series: [big, big, big, big, big, big] } },
    )
    expect(document.querySelector('figure')?.innerHTML).not.toMatch(/NaN|Infinity/)
  })

  test('labels, keys and units are only ever text: nothing is loaded, linked, styled or run', async () => {
    const hostile = [
      '<img src="https://evil.example/x" onerror="alert(1)">',
      'javascript:alert(1)',
      'url(https://evil.example/p)',
      '"><script>alert(1)</script>',
      '</text><image href="https://evil.example/i"/>',
      '__proto__',
      'constructor',
    ]
    const x = hostile.map((label) => ({ key: label, label }))
    const series = hostile.slice(0, 3).map((label) => ({ key: label, label, values: hostile.map((_, i) => i + 1) }))
    for (const variant of ['line', 'bar', 'area']) {
      cleanup()
      await drawn({ variant, stacked: true, unit: '<b onclick=1>', label: hostile[0] }, { by_stage: { x, series } })
      const figure = screen.getByRole('figure')
      const allowed: Record<string, string[]> = {
        figure: ['class'],
        figcaption: ['class'],
        svg: ['class', 'viewBox', 'role', 'aria-label', 'focusable', 'direction'],
        g: [],
        line: ['class', 'x1', 'x2', 'y1', 'y2'],
        text: ['class', 'x', 'y', 'text-anchor'],
        path: ['class', 'd'],
        rect: ['class', 'x', 'y', 'width', 'height'],
        circle: ['class', 'cx', 'cy', 'r'],
        ul: ['class'],
        li: ['class'],
        span: ['class', 'aria-hidden'],
        table: [],
        caption: [],
        thead: [],
        tbody: [],
        tr: [],
        th: ['scope'],
        td: [],
        p: ['class'],
      }
      for (const el of [figure, ...figure.querySelectorAll('*')]) {
        const tag = el.tagName
        expect(Object.keys(allowed), tag).toContain(tag.toLowerCase())
        for (const attr of el.getAttributeNames()) expect(allowed[tag.toLowerCase()], `${tag} ${attr}`).toContain(attr)
        for (const cls of el.classList) expect(cls).toMatch(/^ui-chart(-[a-z]+)*(-s[1-6])?$|^ui-visually-hidden$/)
        if (tag === 'path') expect(el.getAttribute('d')).toMatch(/^[MLZ0-9. -]*$/)
        if (['rect', 'circle', 'line'].includes(tag.toLowerCase()))
          for (const attr of el.getAttributeNames())
            if (attr !== 'class') expect(el.getAttribute(attr), attr).toMatch(/^-?\d+(\.\d+)?$/)
      }
      expect(figure.querySelector('img, script, image, a, use, foreignObject, style, iframe')).toBeNull()
      expect(within(figure).getAllByText(hostile[0], { exact: true, ignore: 'none' }).length).toBeGreaterThan(0)
    }
  })

  test('the chart schema takes no prop that could point outside the app', () => {
    const base = { data: { path: '/by_stage' }, label: 'x' }
    expect(ChartApi.schema.safeParse(base).success).toBe(true)
    for (const extra of [
      { href: 'https://x' },
      { style: 'x' },
      { colors: ['black'] },
      { src: 'x' },
      { variant: 'pie' },
    ])
      expect(ChartApi.schema.safeParse({ ...base, ...extra }).success).toBe(false)
  })
})

describe('what people read', () => {
  const keyed = {
    x: [
      { key: 'false', label: 'false' },
      { key: 'true', label: 'true' },
      { key: 'other', label: 'other' },
      { key: '2026-11-27', label: '2026-11-27' },
      { key: '2026-11', label: '2026-11' },
      { key: '2026-W48', label: '2026-W48' },
      { key: '2026-13-45', label: '2026-13-45' },
      { key: 'urgent', label: 'Urgent!' },
    ],
    series: [{ key: '', label: '', values: [1, 2, 3, 4, 5, 6, 7, 8] }],
  }

  test('the keys the server writes are translated and its dates formatted, in English', async () => {
    await drawn({ variant: 'bar' }, { by_stage: keyed })
    expect(tableRows().map((r) => r[0])).toEqual([
      'Group',
      'No',
      'Yes',
      'Other',
      'Nov 27, 2026',
      'Nov 2026',
      'Week 48, 2026',
      '2026-13-45',
      'Urgent!',
    ])
  })

  test('a label the server derived from a choice or the app wrote is shown as it is, whatever its key looks like', async () => {
    const x = [
      { key: 'true', label: 'Approved' },
      { key: '2026-11', label: 'Q4' },
      { key: 'other', label: 'Misc' },
      { key: '2026-11-27', label: 'Launch day' },
    ]
    await drawn({ variant: 'bar' }, { by_stage: { x, series: [{ key: '', label: '', values: [1, 2, 3, 4] }] } })
    expect(tableRows().map((r) => r[0])).toEqual(['Group', 'Approved', 'Q4', 'Misc', 'Launch day'])
  })

  test('and in Portuguese', async () => {
    await setPreferences({ user: 'pt-BR' })
    await drawn({ variant: 'bar' }, { by_stage: keyed })
    expect(tableRows().map((r) => r[0])).toEqual([
      'Grupo',
      'Não',
      'Sim',
      'Outros',
      '27 de nov. de 2026',
      'nov. de 2026',
      'Semana 48, 2026',
      '2026-13-45',
      'Urgent!',
    ])
    expect(screen.getByRole('table', { hidden: true }).querySelector('caption')?.textContent).toBe(
      'Dados de Deals per stage',
    )
  })

  test('numbers follow the language', async () => {
    await setPreferences({ user: 'pt-BR' })
    await drawn({ variant: 'bar' }, { by_stage: stages })
    expect(tableRows()[2]).toEqual(['Won', '1.234,5'])
  })

  test('a series with no name is called Value, and one whose name is a key is translated', async () => {
    await drawn(
      { variant: 'bar' },
      {
        by_stage: {
          x: stages.x,
          series: [
            { key: '', label: '', values: [1, 2, 3] },
            { key: 'true', label: 'true', values: [1, 2, 3] },
          ],
        },
      },
    )
    expect(tableRows()[0]).toEqual(['Group', 'Value', 'Yes'])
  })
})

const same = (count: number, chars: number) => Array.from({ length: count }, () => chars * 7)

describe('the numbers behind the picture', () => {
  test('niceMax rounds up to 1, 2, 2.5, 5 or 10 times a power of ten', () => {
    expect([0.3, 1, 1.1, 2, 2.2, 3, 5, 7, 11, 24, 26, 99, 1234].map(niceMax)).toEqual([
      0.5, 1, 2, 2, 2.5, 5, 5, 10, 20, 25, 50, 100, 2000,
    ])
    expect(niceMax(0)).toBeGreaterThan(0)
  })

  test('the scale starts at zero, ends on a nice number and has evenly spaced ticks', () => {
    expect(chartScale([3, 1234.5, 0])).toEqual({ min: 0, max: 1500, ticks: [0, 500, 1000, 1500] })
    expect(chartScale([0, 0])).toEqual({ min: 0, max: 1, ticks: [0, 0.25, 0.5, 0.75, 1] })
    expect(chartScale([]).max).toBeGreaterThan(0)
    expect(chartScale([-30, 10])).toEqual({ min: -30, max: 10, ticks: [-30, -20, -10, 0, 10] })
  })

  test('the scale holds a value just across zero, with no tolerance, on either side', () => {
    expect(chartScale([1, -1e-13]).min).toBeLessThan(0)
    expect(chartScale([-1, 1e-13]).max).toBeGreaterThan(0)
    expect(chartScale([1e15, -1e-3]).min).toBeLessThan(0)
    expect(chartScale([-1e15, 1e-3]).max).toBeGreaterThan(0)
    expect(chartScale([1e-13, -1]).max).toBeGreaterThan(1e-13)
    expect(chartScale([-1e-13, 1]).min).toBeLessThan(0)
  })

  test('the scale never leaves out a value, however small it is beside the others', () => {
    expect(chartScale([1, -1e-10]).min).toBeLessThan(0)
    expect(chartScale([-1, 1e-10]).max).toBeGreaterThan(0)
    const inputs = [
      [1, -1e-10],
      [-1, 1e-10],
      [1e-10, -1],
      [0.1 + 0.2, 0.3],
      [1e15, -1e-3],
      [-7.5, 2.4999999999, 1e-12],
      [24.999999999, -0.0000001],
      [2e-9, 1],
      [1, -1e-13],
      [-1, 1e-13],
      [1e-13, -1],
      [-1e-13, 1],
      [1, -1e-14],
      [-1, 1e-14],
      [0.1 + 0.2],
      [-(0.1 + 0.2)],
    ]
    for (const values of inputs) {
      const scale = chartScale(values)
      for (const v of values) {
        expect(scale.min).toBeLessThanOrEqual(v)
        expect(scale.max).toBeGreaterThanOrEqual(v)
      }
      expect(scale.ticks[0]).toBe(scale.min)
      expect(scale.ticks.at(-1)).toBe(scale.max)
    }
  })

  test('stacked layers add up, a null adds nothing and has no layer of its own', () => {
    const lines: ChartLine[] = [
      { key: 'a', label: 'a', values: [1, null, 3] },
      { key: 'b', label: 'b', values: [2, 2, null] },
    ]
    expect(stackLayers(lines)).toEqual([
      [
        { from: 0, to: 1, has: true },
        { from: 0, to: 0, has: false },
        { from: 0, to: 3, has: true },
      ],
      [
        { from: 1, to: 3, has: true },
        { from: 0, to: 2, has: true },
        { from: 3, to: 3, has: false },
      ],
    ])
    expect(plotted(lines, true)).toEqual([0, 1, 0, 3, 1, 3, 0, 2])
    expect(plotted(lines, false)).toEqual([1, 3, 2, 2])
  })

  test('runs are the stretches of values with no gap', () => {
    expect(runs([1, 2, null, 3, null, null, 4, 5])).toEqual([
      { start: 0, end: 1 },
      { start: 3, end: 3 },
      { start: 6, end: 7 },
    ])
    expect(runs([null])).toEqual([])
  })

  test('names that fit are all shown, otherwise evenly spaced ones, never more than three for many points', () => {
    expect(axisPicks(4, same(4, 6))).toEqual([0, 1, 2, 3])
    expect(axisPicks(7, same(7, 3))).toEqual([0, 3, 6])
    expect(axisPicks(12, same(12, 3))).toEqual([0, 6, 11])
    expect(axisPicks(4, same(4, 14))).toEqual([0, 3])
    expect(axisPicks(5, same(5, 14))).toEqual([0, 4])
    expect(axisPicks(2, same(2, 14))).toEqual([0, 1])
    expect(axisPicks(6, same(6, 60))).toEqual([0])
  })

  test('names never overlap, whatever their anchor and width, and the last gap is not left short', () => {
    expect(axisPicks(4, same(4, 13))).toEqual([0, 3])
    const width = 272
    const extent = (i: number, count: number, w: number, band: boolean) => {
      const x = band ? ((i + 0.5) * width) / count : count === 1 ? width / 2 : (i * width) / (count - 1)
      if (count === 1) return [x - w / 2, x + w / 2]
      if (i === 0) return [x, x + w]
      if (i === count - 1) return [x - w, x]
      return [x - w / 2, x + w / 2]
    }
    for (const band of [false, true]) {
      for (let count = 2; count <= 40; count++) {
        for (let chars = 1; chars <= 14; chars++) {
          const widths = Array.from({ length: count }, (_, i) => (i % 3 === 1 ? 0 : ((i * 5) % chars) + 1) * 7)
          for (const set of [same(count, chars), widths]) {
            const picks = axisPicks(count, set, band)
            expect(picks[0]).toBe(0)
            const boxes = picks.map((i) => extent(i, count, set[i], band))
            for (let k = 1; k < boxes.length; k++) expect(boxes[k][0] - boxes[k - 1][1]).toBeGreaterThanOrEqual(10)
            const gaps = picks.slice(1).map((p, k) => p - picks[k])
            if (gaps.length > 1) expect(Math.min(...gaps) * 4).toBeGreaterThanOrEqual(Math.max(...gaps) * 3)
          }
        }
      }
    }
  })

  test('a name is never drawn narrower than Inter draws it, nor much wider', () => {
    const tokens = readFileSync('src/ui/tokens.css', 'utf8')
    expect(tokens).toContain('--text-xs: 12px;')
    expect(tokens).toContain('--type-caption: 400 var(--text-xs)')
    expect(TICK_SIZE).toBe(12)
    for (const [ch, em] of Object.entries(INTER_ADVANCE_EM)) {
      const drawnWidth = em * TICK_SIZE
      expect(labelWidth(ch), ch).toBeGreaterThanOrEqual(drawnWidth)
      expect(labelWidth(ch), ch).toBeLessThanOrEqual(drawnWidth + 0.25 * TICK_SIZE)
    }
    for (const [text, px] of INTER_STRING_PX) {
      expect(labelWidth(text), text).toBeGreaterThanOrEqual(px)
      expect(labelWidth(text), text).toBeLessThanOrEqual(px * 1.15)
    }
    expect(labelWidth('é')).toBeGreaterThan(0)
    expect(labelWidth('\u0301')).toBe(0)
    expect(labelWidth('日')).toBeGreaterThanOrEqual(TICK_SIZE)
  })

  test('reading takes only own properties and keeps the series in step with the points', () => {
    const inherited = Object.create({ x: stages.x, series: stages.series })
    expect(readChart(inherited)).toBeNull()
    const read = readChart({
      x: [{ key: 'a' }, 4, { key: 'c', label: 'C' }],
      series: [{ values: [1] }, { label: 'long', values: [1, 2, 3, 4, 5] }],
    })
    expect(read?.x).toEqual([
      { key: 'a', label: 'a' },
      { key: '', label: '' },
      { key: 'c', label: 'C' },
    ])
    expect(read?.series.map((s) => s.values)).toEqual([
      [1, null, null],
      [1, 2, 3],
    ])
    const many = readChart({
      x: [{ key: 'a' }],
      series: Array.from({ length: 50 }, () => ({ values: [1] })),
    })
    expect(many?.series).toHaveLength(MAX_SERIES_LISTED)
  })
})

const AXIS_NAME_Y = '174'
const CAPTION_SIZE = 12

function inter(text: string): number {
  return Array.from(text).reduce((sum, ch) => sum + INTER_ADVANCE_EM[ch] * CAPTION_SIZE, 0)
}

function reach(text: Element): [number, number] {
  const label = text.textContent ?? ''
  const x = Number(text.getAttribute('x'))
  const w = inter(label)
  const anchor = text.getAttribute('text-anchor')
  if (anchor === 'end') return [x - w, x]
  if (anchor === 'middle') return [x - w / 2, x + w / 2]
  return [x, x + w]
}

const ticks = () => [...document.querySelectorAll('svg text.ui-chart-tick')]
const axisNames = () => ticks().filter((t) => t.getAttribute('y') === AXIS_NAME_Y && (t.textContent ?? '') !== '')
const valueTicks = () => ticks().filter((t) => t.getAttribute('y') !== AXIS_NAME_Y)

function points(labels: string[]) {
  return {
    x: labels.map((label) => ({ key: label, label })),
    series: [{ key: '', label: '', values: labels.map((_, i) => i + 1) }],
  }
}

describe('what is drawn stays apart and inside, as Inter sets it', () => {
  test('axis names clear one another, the wide ones between narrow ones too', async () => {
    await drawn({ variant: 'line' }, { by_stage: points(['WWWWWWWWW', '', 'WWWWWWWWW', '', 'Z']) })
    const boxes = axisNames()
      .map(reach)
      .sort((a, b) => a[0] - b[0])
    expect(boxes.length).toBeGreaterThan(1)
    for (let k = 1; k < boxes.length; k++) expect(boxes[k][0] - boxes[k - 1][1]).toBeGreaterThan(0)
  })

  test('axis names clear one another for bars and lines, whatever they say and however many', async () => {
    const kinds = [
      (i: number) => 'WWWWWWWWWWWWWW'.slice(0, 4 + (i % 10)),
      (i: number) => `2026-${String((i % 12) + 1).padStart(2, '0')}-28`,
      (i: number) => ['i', 'Marketing', 'W', 'mm', 'Quarterly review', 'x'][i % 6],
    ]
    for (const variant of ['line', 'bar']) {
      for (const total of [2, 3, 5, 8, 13]) {
        for (const kind of kinds) {
          const labels = Array.from({ length: total }, (_, i) => kind(i))
          await drawn({ variant }, { by_stage: points(labels) })
          const names = axisNames()
          const boxes = names.map(reach).sort((a, b) => a[0] - b[0])
          const where = `${variant} ${total} ${labels.join('|')}`
          expect(boxes.length, where).toBeGreaterThan(0)
          for (let k = 1; k < boxes.length; k++) expect(boxes[k][0] - boxes[k - 1][1], where).toBeGreaterThan(0)
          cleanup()
        }
      }
    }
  })

  test('the scale ticks stay inside the picture and left of the plot, even for tiny and huge numbers', async () => {
    for (const values of [
      [2e-9, 3e-9],
      [1e15, -1e15],
      [0.0001, 0.0003],
      [3, 1234.5, 0],
    ]) {
      const labels = values.map((_, i) => `p${i}`)
      await drawn({ variant: 'line' }, { by_stage: { x: points(labels).x, series: [{ key: '', label: '', values }] } })
      const left = Number(document.querySelector('svg line')?.getAttribute('x1'))
      const shown = valueTicks()
      expect(shown.length).toBeGreaterThan(2)
      for (const tick of shown) {
        const [from, to] = reach(tick)
        expect(from, `${values} ${tick.textContent}`).toBeGreaterThanOrEqual(0)
        expect(to, `${values} ${tick.textContent}`).toBeLessThanOrEqual(left)
      }
      cleanup()
    }
  })

  test('what is drawn is as wide as the room left after the ticks', async () => {
    await drawn(
      { variant: 'bar' },
      { by_stage: { x: points(['a', 'b']).x, series: [{ key: '', label: '', values: [2e-9, 3e-9] }] } },
    )
    const left = Number(document.querySelector('svg line')?.getAttribute('x1'))
    expect(left).toBeGreaterThan(40)
    const rects = [...document.querySelectorAll('svg rect')].map((r) => ({
      from: Number(r.getAttribute('x')),
      to: Number(r.getAttribute('x')) + Number(r.getAttribute('width')),
    }))
    for (const r of rects) {
      expect(r.from).toBeGreaterThanOrEqual(left)
      expect(r.to).toBeLessThanOrEqual(312)
    }
  })
})

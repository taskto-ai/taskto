// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, test } from 'vitest'
import { AppView } from './AppView'
import { APPS_CATALOG_ID } from './catalog'
import { iconPath, iconsShowingTheirName, MAX_ICON_PATH, type SurfaceComponent } from './iconPath'

afterEach(cleanup)

const PLUS = 'M10 4h4v6h6v4h-6v6h-4v-6H4v-4h6z'
const BACK = 'M3 12l8-8v5h10v6H11v5z'
const BOUND = 'm0 0h24v24H0z'

const NOT_PATHS = [
  '',
  'M0 0<script>alert(1)</script>',
  'M0 0"><img src=x onerror=alert(1)>',
  'javascript:alert(1)',
  'data:image/svg+xml;base64,PHN2Zz4=',
  'http://elsewhere.example/icon.svg',
  'M0 0 url(http://elsewhere.example/x)',
  "M0 0'",
  'M0 0 X1 1',
  'L0 0',
  ' M0 0',
  'M0 0\nL1 1',
  `M${'0'.repeat(MAX_ICON_PATH)}`,
]

test('the grammar takes path data and nothing else', () => {
  for (const d of [PLUS, BACK, BOUND, 'M1.5e1-2E-1,3 4Z', `M${'0'.repeat(MAX_ICON_PATH - 1)}`])
    expect(iconPath({ svgPath: d })).toBe(d)
  for (const d of NOT_PATHS) expect(iconPath({ svgPath: d })).toBeNull()
  for (const name of ['add', 'star', null, undefined, 5, ['M0 0'], { path: '/x' }, { svgPath: 5 }])
    expect(iconPath(name)).toBeNull()
  expect(iconPath({ svgPath: PLUS, href: 'http://elsewhere.example' })).toBeNull()
})

function messages() {
  const bad = NOT_PATHS.map((d, i) => ({ id: `bad-${i}`, component: 'Icon', name: { svgPath: d } }))
  return [
    { version: 'v0.9', createSurface: { surfaceId: 'icons', catalogId: APPS_CATALOG_ID } },
    {
      version: 'v0.9',
      updateComponents: {
        surfaceId: 'icons',
        components: [
          {
            id: 'root',
            component: 'Column',
            children: ['beside', 'add', 'named', 'bound', 'bound-bad', ...bad.map((b) => b.id)],
          },
          { id: 'beside', component: 'Row', children: ['back-icon', 'back-text'] },
          { id: 'back-icon', component: 'Icon', name: { svgPath: BACK } },
          { id: 'back-text', component: 'Text', text: 'Back' },
          { id: 'add', component: 'Button', child: 'add-icon', action: { event: { name: 'add' } } },
          { id: 'add-icon', component: 'Icon', name: { svgPath: PLUS }, accessibility: { label: 'Add' } },
          { id: 'named', component: 'Icon', name: 'star' },
          { id: 'bound', component: 'Icon', name: { path: '/icon' } },
          { id: 'bound-bad', component: 'Icon', name: { path: '/badIcon' } },
          ...bad,
        ],
      },
    },
    {
      version: 'v0.9',
      updateDataModel: {
        surfaceId: 'icons',
        path: '/',
        value: { icon: { svgPath: BOUND }, badIcon: { svgPath: 'M0 0"/><script>alert(1)</script>' } },
      },
    },
  ]
}

test('draws valid paths in its own svg and nothing for the rest', async () => {
  const { container } = render(
    <AppView surfaceId="icons" label="Icons" transport={{ load: async () => messages(), act: async () => [] }} />,
  )
  await screen.findByText('Back')
  await waitFor(() => expect(container.querySelectorAll('svg.ui-app-icon')).toHaveLength(3))

  const drawn = [...container.querySelectorAll('svg.ui-app-icon')]
  expect(drawn.map((svg) => svg.querySelector('path')?.getAttribute('d')).sort()).toEqual([BACK, BOUND, PLUS].sort())
  for (const svg of drawn) {
    expect(svg.getAttribute('viewBox')).toBe('0 0 24 24')
    expect(svg.getAttribute('fill')).toBe('currentColor')
    expect(svg.children).toHaveLength(1)
    expect([...(svg.querySelector('path')?.attributes ?? [])].map((a) => a.name)).toEqual(['d'])
  }

  expect(screen.getByRole('button', { name: 'Add' })).toBeTruthy()
  expect(screen.getByRole('img', { name: 'Add' }).querySelector('path')?.getAttribute('d')).toBe(PLUS)
  const back = drawn.find((svg) => svg.querySelector('path')?.getAttribute('d') === BACK)
  expect(back?.getAttribute('aria-hidden')).toBe('true')
  expect(back?.hasAttribute('role')).toBe(false)

  expect(screen.queryByText('star')).toBeNull()
  expect(container.querySelector('img, script, i.material-icons')).toBeNull()
  expect(container.innerHTML).not.toContain('alert(1)')
  expect(container.innerHTML).not.toContain('elsewhere.example')
})

test('a name shows its word only where nothing else names the control', () => {
  const c = (component: string, props: Record<string, unknown> = {}): SurfaceComponent => ({ component, props })
  const named = c('Icon', { name: 'delete' })
  const path = c('Icon', { name: { svgPath: PLUS } })
  const labelled = c('Icon', { name: { svgPath: PLUS }, accessibility: { label: 'Add' } })
  const cases: [string, Record<string, SurfaceComponent>, string[]][] = [
    ['alone in a button', { b: c('Button', { child: 'n' }), n: named }, ['n']],
    [
      'a modal trigger',
      { m: c('Modal', { trigger: 'n', content: 't' }), n: named, t: c('Text', { text: 'Hi' }) },
      ['n'],
    ],
    [
      'through a card and a row, beside a path',
      {
        b: c('Button', { child: 'k' }),
        k: c('Card', { child: 'r' }),
        r: c('Row', { children: ['p', 'n'] }),
        p: path,
        n: named,
      },
      ['n'],
    ],
    [
      'beside a blank text and a divider',
      {
        b: c('Button', { child: 'r' }),
        r: c('Row', { children: ['n', 't', 'd'] }),
        n: named,
        t: c('Text', { text: ' ' }),
        d: c('Divider'),
      },
      ['n'],
    ],
    [
      'beside text',
      {
        b: c('Button', { child: 'r' }),
        r: c('Row', { children: ['n', 't'] }),
        n: named,
        t: c('Text', { text: 'Delete' }),
      },
      [],
    ],
    [
      'beside a labelled path',
      { b: c('Button', { child: 'r' }), r: c('Row', { children: ['n', 'p'] }), n: named, p: labelled },
      [],
    ],
    ['not in a control', { r: c('Row', { children: ['n'] }), n: named }, []],
    [
      'beside a bound text',
      {
        b: c('Button', { child: 'r' }),
        r: c('Row', { children: ['n', 't'] }),
        n: named,
        t: c('Text', { text: { path: '/x' } }),
      },
      [],
    ],
  ]
  for (const [name, components, want] of cases)
    expect([...iconsShowingTheirName(new Map(Object.entries(components)))], name).toEqual(want)
})

test('the walk does not multiply references that fan out', () => {
  const fan = (next: string) => ({ component: 'Row', props: { children: Array(200).fill(next) } })
  const components = new Map<string, SurfaceComponent>([
    ['b', { component: 'Button', props: { child: 'r1' } }],
    ['r1', fan('r2')],
    ['r2', fan('r3')],
    ['r3', fan('r4')],
    ['r4', { component: 'Row', props: { children: ['n'] } }],
    ['n', { component: 'Icon', props: { name: 'delete' } }],
  ])
  expect([...iconsShowingTheirName(components)]).toEqual(['n'])
})

test('a control that is only a name keeps its word, its name and its use', async () => {
  const messages = [
    { version: 'v0.9', createSurface: { surfaceId: 'named', catalogId: APPS_CATALOG_ID } },
    {
      version: 'v0.9',
      updateComponents: {
        surfaceId: 'named',
        components: [
          { id: 'root', component: 'Column', children: ['del', 'more', 'title-row'] },
          { id: 'del', component: 'Button', child: 'del-icon', action: { event: { name: 'remove' } } },
          { id: 'del-icon', component: 'Icon', name: 'delete' },
          { id: 'more', component: 'Modal', trigger: 'more-icon', content: 'more-body' },
          { id: 'more-icon', component: 'Icon', name: 'menu' },
          { id: 'more-body', component: 'Text', text: 'More options' },
          { id: 'title-row', component: 'Row', children: ['star', 'title'] },
          { id: 'star', component: 'Icon', name: 'star' },
          { id: 'title', component: 'Text', text: 'Favourites' },
        ],
      },
    },
  ]
  const { container } = render(
    <AppView surfaceId="named" label="Named" transport={{ load: async () => messages, act: async () => [] }} />,
  )
  await screen.findByText('Favourites')
  expect(screen.getByRole('button', { name: 'delete' }).textContent).toContain('delete')
  expect(screen.queryByText('star')).toBeNull()
  expect(container.querySelectorAll('.ui-app-icon-name')).toHaveLength(2)
  fireEvent.click(screen.getByText('menu'))
  await screen.findByText('More options')
})

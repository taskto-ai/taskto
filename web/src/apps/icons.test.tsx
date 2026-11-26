// @vitest-environment jsdom
//
// An app's icons are SVG path data from its definition (internal/apps/icons.go). The renderer draws
// a valid path in an <svg> it makes itself, and nothing for a name or for anything that isn't path
// data, even when the definition reached it without the server's check.

import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, test } from 'vitest'
import { AppView } from './AppView'
import { APPS_CATALOG_ID } from './catalog'
import { iconPath, MAX_ICON_PATH } from './iconPath'

afterEach(cleanup)

const PLUS = 'M11 5h2v6h6v2h-6v6h-2v-6H5v-2h6z'
const BACK = 'M20 11H7.8l5.6-5.6L12 4l-8 8 8 8 1.4-1.4L7.8 13H20z'
const BOUND = 'm0 0h24v24H0z'

// What isn't path data, as the server refuses it; here it is drawn as nothing.
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
  // A name, a bound value that isn't {svgPath}, and an object with more keys draw nothing.
  for (const name of ['add', 'star', null, undefined, 5, ['M0 0'], { path: '/x' }, { svgPath: 5 }])
    expect(iconPath(name)).toBeNull()
  expect(iconPath({ svgPath: PLUS, href: 'http://elsewhere.example' })).toBeNull()
})

// A surface the server never checked: a valid icon beside a label, one that is all of a button, a
// name, a bound path, and every kind of string that isn't path data.
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
    // The svg and one path, with no attribute from the definition but d.
    expect(svg.children).toHaveLength(1)
    expect([...(svg.querySelector('path')?.attributes ?? [])].map((a) => a.name)).toEqual(['d'])
  }

  // The icon that is all of the button names it; the one beside a label is hidden.
  expect(screen.getByRole('button', { name: 'Add' })).toBeTruthy()
  expect(screen.getByRole('img', { name: 'Add' }).querySelector('path')?.getAttribute('d')).toBe(PLUS)
  const back = drawn.find((svg) => svg.querySelector('path')?.getAttribute('d') === BACK)
  expect(back?.getAttribute('aria-hidden')).toBe('true')
  expect(back?.hasAttribute('role')).toBe(false)

  // A name shows neither an icon nor the word, and nothing that isn't path data got anywhere.
  expect(screen.queryByText('star')).toBeNull()
  expect(container.querySelector('img, script, i.material-icons')).toBeNull()
  expect(container.innerHTML).not.toContain('alert(1)')
  expect(container.innerHTML).not.toContain('elsewhere.example')
})

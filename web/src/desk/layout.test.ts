import { expect, test } from 'vitest'
import {
  activate,
  closeTab,
  findTabByPath,
  focusedTab,
  focusGroup,
  type GroupNode,
  groupsOf,
  isGroup,
  type Layout,
  type LayoutNode,
  MAX_CHILDREN,
  MAX_GROUPS,
  MAX_TABS,
  MAX_TABS_PER_GROUP,
  navigateTab,
  openTab,
  readLayout,
  single,
  splitGroup,
  tabCount,
} from './layout'

const p = (name: string) => `/w/1/${name}`
const group = (g: string, ...tabs: [string, string][]): GroupNode => ({
  group: g,
  active: tabs[0][0],
  tabs: tabs.map(([id, path]) => ({ id, path: p(path) })),
})
const row = (sizes: number[], ...children: LayoutNode[]): LayoutNode => ({ split: 'row', sizes, children })
const desk = (root: LayoutNode, focus: string): Layout => ({ v: 1, focus, root })
const twoPanes = () =>
  desk(row([0.5, 0.5], group('ga', ['ta', 'home'], ['tb', 'boards']), group('gb', ['tc', 'apps'])), 'ga')
const paths = (l: Layout) => groupsOf(l).map((g) => g.tabs.map((x) => x.path))

test('single makes one pane with one tab that is focused', () => {
  const l = single(p('home'))
  expect(groupsOf(l)).toHaveLength(1)
  expect(focusedTab(l)?.path).toBe(p('home'))
  expect(l.focus).toBe(groupsOf(l)[0].group)
})

test('opening a tab adds it to the group, activates it and focuses the group', () => {
  const start = focusGroup(twoPanes(), 'ga').layout
  const { layout, reason } = openTab(start, 'gb', p('settings'))
  expect(reason).toBeUndefined()
  expect(layout.focus).toBe('gb')
  expect(paths(layout)[1]).toEqual([p('apps'), p('settings')])
  expect(focusedTab(layout)?.path).toBe(p('settings'))
})

test('a group takes at most 20 tabs, and the desk 60, and a refused open leaves the layout as it was', () => {
  let l = single(p('home'))
  const first = groupsOf(l)[0].group
  for (let i = 1; i < MAX_TABS_PER_GROUP; i++) l = openTab(l, first, p('boards')).layout
  expect(tabCount(l)).toBe(MAX_TABS_PER_GROUP)
  const refused = openTab(l, first, p('boards'))
  expect(refused.reason).toBe('too_many_tabs')
  expect(refused.layout).toBe(l)

  let wide = single(p('home'))
  for (let i = 1; i < MAX_GROUPS; i++) wide = splitGroup(wide, wide.focus, 'inline-end', p('home')).layout
  for (const g of groupsOf(wide)) {
    for (let i = 1; i < MAX_TABS_PER_GROUP && tabCount(wide) < MAX_TABS; i++)
      wide = openTab(wide, g.group, p('apps')).layout
  }
  expect(tabCount(wide)).toBe(MAX_TABS)
  expect(openTab(wide, wide.focus, p('apps')).reason).toBe('too_many_tabs')
})

test('splitting puts the new pane on the asked side, focuses it, and keeps the sizes valid', () => {
  const start = single(p('home'))
  const first = start.focus
  const after = splitGroup(start, first, 'inline-end', p('boards')).layout
  const root = after.root as LayoutNode
  expect(isGroup(root)).toBe(false)
  if (!isGroup(root)) {
    expect(root.split).toBe('row')
    expect(root.sizes).toEqual([0.5, 0.5])
    expect((root.children[0] as GroupNode).group).toBe(first)
  }
  expect(after.focus).not.toBe(first)
  expect(focusedTab(after)?.path).toBe(p('boards'))
  const before = splitGroup(start, first, 'inline-start', p('boards')).layout
  expect(paths(before)).toEqual([[p('boards')], [p('home')]])
  const stacked = splitGroup(start, first, 'block-end', p('boards')).layout
  expect((stacked.root as { split: string }).split).toBe('column')
})

test('splitting along the axis of the parent adds a sibling and halves the size of the one split', () => {
  const l = desk(row([0.4, 0.6], group('ga', ['ta', 'home']), group('gb', ['tb', 'boards'])), 'ga')
  const next = splitGroup(l, 'gb', 'inline-end', p('apps')).layout
  const root = next.root as { sizes: number[]; children: LayoutNode[] }
  expect(root.children).toHaveLength(3)
  expect(root.sizes[0]).toBe(0.4)
  expect(root.sizes[1]).toBeCloseTo(0.3)
  expect(root.sizes[2]).toBeCloseTo(0.3)
})

test('a split refuses a seventh pane, a fifth sibling and a fourth level', () => {
  let l = single(p('home'))
  for (let i = 1; i < MAX_GROUPS; i++) l = splitGroup(l, l.focus, 'inline-end', p('home')).layout
  expect(groupsOf(l)).toHaveLength(MAX_GROUPS)
  const seventh = splitGroup(l, l.focus, 'inline-end', p('home'))
  expect(seventh.reason).toBe('too_many_groups')
  expect(seventh.layout).toBe(l)

  let siblings = single(p('home'))
  for (let i = 1; i < MAX_CHILDREN; i++) siblings = splitGroup(siblings, siblings.focus, 'inline-end', p('home')).layout
  expect((siblings.root as { children: LayoutNode[] }).children).toHaveLength(MAX_CHILDREN)
  const fifth = splitGroup(siblings, siblings.focus, 'inline-end', p('home')).layout
  expect((fifth.root as { children: LayoutNode[] }).children).toHaveLength(MAX_CHILDREN)
  expect(groupsOf(fifth)).toHaveLength(MAX_CHILDREN + 1)

  let deep = single(p('home'))
  deep = splitGroup(deep, deep.focus, 'inline-end', p('home')).layout
  deep = splitGroup(deep, deep.focus, 'block-end', p('home')).layout
  expect(splitGroup(deep, deep.focus, 'inline-end', p('home')).reason).toBe('too_deep')
})

test('closing the active tab activates its neighbour; closing another keeps the active one', () => {
  const l = desk(
    row([0.5, 0.5], group('ga', ['ta', 'home'], ['tb', 'boards'], ['tc', 'apps']), group('gb', ['td', 'settings'])),
    'ga',
  )
  const mid = activate(l, 'tb').layout
  expect(groupsOf(closeTab(mid, 'tb').layout)[0].active).toBe('tc')
  const last = activate(l, 'tc').layout
  expect(groupsOf(closeTab(last, 'tc').layout)[0].active).toBe('tb')
  expect(groupsOf(closeTab(mid, 'ta').layout)[0].active).toBe('tb')
})

test('closing the last tab of a pane removes it, collapses the split and moves the focus', () => {
  const l = twoPanes()
  const closed = closeTab(focusGroup(l, 'gb').layout, 'tc').layout
  expect(isGroup(closed.root as LayoutNode)).toBe(true)
  expect(closed.focus).toBe('ga')
  expect(paths(closed)).toEqual([[p('home'), p('boards')]])
})

test('closing a pane in a split of three keeps the others and spreads its size over them', () => {
  const l = desk(
    row([0.2, 0.3, 0.5], group('ga', ['ta', 'home']), group('gb', ['tb', 'boards']), group('gc', ['tc', 'apps'])),
    'gb',
  )
  const closed = closeTab(l, 'tb').layout
  const root = closed.root as { sizes: number[]; children: LayoutNode[] }
  expect(root.children).toHaveLength(2)
  expect(root.sizes.reduce((a, b) => a + b, 0)).toBeCloseTo(1)
  expect(root.sizes[0]).toBeCloseTo(0.2 / 0.7, 3)
  expect(closed.focus).toBe('gc')
})

test('the only tab of the desk is never closed', () => {
  const l = single(p('home'))
  const refused = closeTab(l, focusedTab(l)?.id ?? '')
  expect(refused.reason).toBe('last_tab')
  expect(refused.layout).toBe(l)
  expect(closeTab(l, 'nope').reason).toBe('not_found')
})

test('activating a tab focuses its pane, and focusing a missing pane changes nothing', () => {
  const l = twoPanes()
  const next = activate(l, 'tb').layout
  expect(next.focus).toBe('ga')
  expect(groupsOf(next)[0].active).toBe('tb')
  expect(activate(next, 'tb').layout).toBe(next)
  expect(focusGroup(l, 'gb').layout.focus).toBe('gb')
  expect(focusGroup(l, 'zz').layout).toBe(l)
  expect(focusGroup(l, 'zz').reason).toBe('not_found')
})

test('navigating a tab changes only its path, and a refused path keeps the layout', () => {
  const l = twoPanes()
  const next = navigateTab(l, 'tc', p('settings')).layout
  expect(paths(next)).toEqual([[p('home'), p('boards')], [p('settings')]])
  expect(navigateTab(l, 'tc', p('apps')).layout).toBe(l)
  const refused = navigateTab(l, 'tc', 'https://example.com')
  expect(refused.reason).toBe('invalid_path')
  expect(refused.layout).toBe(l)
})

test('findTabByPath finds the first tab with exactly that path', () => {
  const l = twoPanes()
  expect(findTabByPath(l, p('apps'))).toEqual({ tabId: 'tc', groupId: 'gb' })
  expect(findTabByPath(l, p('boards'))).toEqual({ tabId: 'tb', groupId: 'ga' })
  expect(findTabByPath(l, p('apps/1'))).toBeNull()
  expect(findTabByPath(l, '/w/2/apps')).toBeNull()
})

test('a layout from the server is read only when it holds every rule and stays in the workspace', () => {
  const good = twoPanes()
  expect(readLayout(JSON.parse(JSON.stringify(good)), 1)).toEqual(good)
  expect(readLayout(good, 2)).toBeNull()
  expect(readLayout({ v: 1, focus: '', root: null }, 1)).toEqual({ v: 1, focus: '', root: null })
  expect(readLayout({ ...good, v: 2 }, 1)).toBeNull()
  expect(readLayout({ ...good, focus: 'zz' }, 1)).toBeNull()
  expect(readLayout(row([0.5], group('ga', ['ta', 'home'])), 1)).toBeNull()
  expect(
    readLayout(
      { v: 1, focus: 'ga', root: row([0.95, 0.05], group('ga', ['ta', 'home']), group('gb', ['tb', 'apps'])) },
      1,
    ),
  ).toBeNull()
  expect(readLayout({ v: 1, focus: 'ga', root: group('ga', ['ta', 'home'], ['ta', 'apps']) }, 1)).toBeNull()
  expect(readLayout({ v: 1, focus: 'ga', root: { ...group('ga', ['ta', 'home']), active: 'zz' } }, 1)).toBeNull()
  expect(readLayout({ v: 1, focus: 'ga', root: { group: 'ga', active: 'ta', tabs: [] } }, 1)).toBeNull()
  expect(readLayout('nope', 1)).toBeNull()
  expect(readLayout({ v: 1, focus: 'ga', root: { group: 'ga' } }, 1)).toBeNull()
})

import { parseRoute } from '../router'

export const MAX_GROUPS = 6
export const MAX_TABS = 60
export const MAX_TABS_PER_GROUP = 20
export const MAX_DEPTH = 3
export const MIN_CHILDREN = 2
export const MAX_CHILDREN = 4
export const MIN_SIZE = 0.1
export const MAX_SIZE = 0.9
const SIZE_SLACK = 0.01

export type Tab = { id: string; path: string }
export type GroupNode = { group: string; active: string; tabs: Tab[] }
export type SplitNode = { split: 'row' | 'column'; sizes: number[]; children: LayoutNode[] }
export type LayoutNode = GroupNode | SplitNode
export type Layout = { v: 1; focus: string; root: LayoutNode | null }
export type Side = 'inline-start' | 'inline-end' | 'block-start' | 'block-end'
export type Reason =
  | 'too_many_groups'
  | 'too_many_tabs'
  | 'too_deep'
  | 'invalid_split'
  | 'invalid_sizes'
  | 'invalid_group'
  | 'invalid_path'
  | 'duplicate_id'
  | 'active'
  | 'focus'
  | 'not_found'
  | 'last_tab'
export type Change = { layout: Layout; reason?: Reason }

export const isGroup = (n: LayoutNode): n is GroupNode => 'group' in n

export function groupsOf(l: Layout): GroupNode[] {
  const walk = (n: LayoutNode): GroupNode[] => (isGroup(n) ? [n] : n.children.flatMap(walk))
  return l.root ? walk(l.root) : []
}

export function tabCount(l: Layout): number {
  return groupsOf(l).reduce((sum, g) => sum + g.tabs.length, 0)
}

export function focusedTab(l: Layout): Tab | null {
  const g = groupsOf(l).find((x) => x.group === l.focus)
  return g?.tabs.find((x) => x.id === g.active) ?? null
}

export function findTabByPath(l: Layout, path: string): { tabId: string; groupId: string } | null {
  for (const g of groupsOf(l)) {
    const found = g.tabs.find((x) => x.path === path)
    if (found) return { tabId: found.id, groupId: g.group }
  }
  return null
}

function violation(l: Layout): Reason | null {
  if (!l.root) return l.focus === '' ? null : 'focus'
  const seen = new Set<string>()
  let groups = 0
  let tabs = 0
  const walk = (n: LayoutNode, depth: number): Reason | null => {
    if (depth > MAX_DEPTH) return 'too_deep'
    if (isGroup(n)) {
      groups += 1
      tabs += n.tabs.length
      if (groups > MAX_GROUPS) return 'too_many_groups'
      if (n.tabs.length === 0) return 'invalid_group'
      if (n.tabs.length > MAX_TABS_PER_GROUP || tabs > MAX_TABS) return 'too_many_tabs'
      for (const key of [n.group, ...n.tabs.map((x) => x.id)]) {
        if (seen.has(key)) return 'duplicate_id'
        seen.add(key)
      }
      if (!n.tabs.some((x) => x.id === n.active)) return 'active'
      return n.tabs.some((x) => parseRoute(x.path) === null) ? 'invalid_path' : null
    }
    if (n.children.length < MIN_CHILDREN || n.children.length > MAX_CHILDREN || n.sizes.length !== n.children.length) {
      return 'invalid_split'
    }
    const sum = n.sizes.reduce((a, b) => a + b, 0)
    if (n.sizes.some((s) => s < MIN_SIZE - 1e-9 || s > MAX_SIZE + 1e-9) || Math.abs(sum - 1) > SIZE_SLACK) {
      return 'invalid_sizes'
    }
    for (const child of n.children) {
      const reason = walk(child, depth + 1)
      if (reason) return reason
    }
    return null
  }
  const reason = walk(l.root, 1)
  if (reason) return reason
  return groupsOf(l).some((g) => g.group === l.focus) ? null : 'focus'
}

export function readLayout(raw: unknown, workspace: number): Layout | null {
  try {
    const layout = raw as Layout
    if (layout.v !== 1 || violation(layout) !== null) return null
    const own = `/w/${workspace}/`
    return groupsOf(layout).every((g) => g.tabs.every((x) => x.path.startsWith(own))) ? layout : null
  } catch {
    return null
  }
}

function freshIds(l: Layout, count: number): string[] {
  const taken = new Set(groupsOf(l).flatMap((g) => [g.group, ...g.tabs.map((x) => x.id)]))
  const made: string[] = []
  while (made.length < count) {
    const next = Math.random().toString(36).slice(2, 10)
    if (next !== '' && !taken.has(next)) {
      taken.add(next)
      made.push(next)
    }
  }
  return made
}

const emptyDesk: Layout = { v: 1, focus: '', root: null }

function newGroup(l: Layout, path: string): GroupNode {
  const [group, tab] = freshIds(l, 2)
  return { group, active: tab, tabs: [{ id: tab, path }] }
}

export function single(path: string): Layout {
  const g = newGroup(emptyDesk, path)
  return { v: 1, focus: g.group, root: g }
}

const refuse = (l: Layout, reason: Reason): Change => ({ layout: l, reason })

function commit(prev: Layout, next: Layout): Change {
  const reason = violation(next)
  return reason ? refuse(prev, reason) : { layout: next }
}

type Visit = (n: LayoutNode) => LayoutNode | null | undefined

function edit(n: LayoutNode, visit: Visit): LayoutNode | null {
  const replaced = visit(n)
  if (replaced !== undefined) return replaced
  if (isGroup(n)) return n
  const kept: { node: LayoutNode; size: number }[] = []
  n.children.forEach((child, i) => {
    const next = edit(child, visit)
    if (next) kept.push({ node: next, size: n.sizes[i] })
  })
  if (kept.length === n.children.length) {
    return kept.every((k, i) => k.node === n.children[i]) ? n : { ...n, children: kept.map((k) => k.node) }
  }
  if (kept.length <= 1) return kept[0]?.node ?? null
  const total = kept.reduce((sum, k) => sum + k.size, 0)
  return { ...n, children: kept.map((k) => k.node), sizes: kept.map((k) => Math.round((k.size / total) * 1e4) / 1e4) }
}

const withGroup = (l: Layout, next: GroupNode, focus = l.focus): Layout => ({
  ...l,
  focus,
  root: l.root ? edit(l.root, (n) => (isGroup(n) && n.group === next.group ? next : undefined)) : null,
})

function locate(l: Layout, tabId: string) {
  const group = groupsOf(l).find((g) => g.tabs.some((x) => x.id === tabId))
  return group ? { group, tab: group.tabs.find((x) => x.id === tabId) as Tab } : null
}

export function focusGroup(l: Layout, groupId: string): Change {
  if (l.focus === groupId) return { layout: l }
  return groupsOf(l).some((g) => g.group === groupId) ? { layout: { ...l, focus: groupId } } : refuse(l, 'not_found')
}

export function activate(l: Layout, tabId: string): Change {
  const found = locate(l, tabId)
  if (!found) return refuse(l, 'not_found')
  if (found.group.active === tabId && l.focus === found.group.group) return { layout: l }
  return { layout: withGroup(l, { ...found.group, active: tabId }, found.group.group) }
}

export function openTab(l: Layout, groupId: string, path: string): Change {
  const group = groupsOf(l).find((g) => g.group === groupId)
  if (!group) return refuse(l, 'not_found')
  const [tab] = freshIds(l, 1)
  return commit(l, withGroup(l, { ...group, active: tab, tabs: [...group.tabs, { id: tab, path }] }, groupId))
}

export function navigateTab(l: Layout, tabId: string, path: string): Change {
  const found = locate(l, tabId)
  if (!found) return refuse(l, 'not_found')
  if (found.tab.path === path) return { layout: l }
  const tabs = found.group.tabs.map((x) => (x.id === tabId ? { ...x, path } : x))
  return commit(l, withGroup(l, { ...found.group, tabs }))
}

export function closeTab(l: Layout, tabId: string): Change {
  const found = locate(l, tabId)
  if (!found) return refuse(l, 'not_found')
  if (tabCount(l) === 1) return refuse(l, 'last_tab')
  const { group } = found
  if (group.tabs.length > 1) {
    const at = group.tabs.findIndex((x) => x.id === tabId)
    const tabs = group.tabs.filter((x) => x.id !== tabId)
    const active = group.active === tabId ? tabs[Math.min(at, tabs.length - 1)].id : group.active
    return commit(l, withGroup(l, { ...group, tabs, active }))
  }
  const before = groupsOf(l)
  const rest = before.filter((g) => g.group !== group.group)
  const focus = l.focus === group.group ? rest[Math.min(before.indexOf(group), rest.length - 1)].group : l.focus
  const root = l.root ? edit(l.root, (n) => (isGroup(n) && n.group === group.group ? null : undefined)) : null
  return commit(l, { ...l, focus, root })
}

export function splitGroup(l: Layout, groupId: string, side: Side, path: string): Change {
  if (!groupsOf(l).some((g) => g.group === groupId)) return refuse(l, 'not_found')
  const axis = side.startsWith('inline') ? 'row' : 'column'
  const later = side.endsWith('end')
  const added = newGroup(l, path)
  const visit: Visit = (n) => {
    if (isGroup(n)) {
      return n.group === groupId
        ? { split: axis, sizes: [0.5, 0.5], children: later ? [n, added] : [added, n] }
        : undefined
    }
    const at = n.children.findIndex((c) => isGroup(c) && c.group === groupId)
    if (at < 0 || n.split !== axis || n.children.length >= MAX_CHILDREN) return undefined
    const children = [...n.children]
    children.splice(later ? at + 1 : at, 0, added)
    const sizes = [...n.sizes]
    sizes.splice(at, 1, n.sizes[at] / 2, n.sizes[at] / 2)
    return { ...n, children, sizes }
  }
  const root = l.root ? edit(l.root, visit) : null
  return commit(l, { ...l, focus: added.group, root })
}

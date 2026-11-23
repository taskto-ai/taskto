// A2UI's inputs draw a <label> beside their <input> without linking the two, so a screen reader
// reads an unnamed field. This gives each such input the name of the label drawn with it: the
// label and the inputs share a parent (a text field, a date or time input), or the label heads a
// group of choices. It never changes an input that already has a name.

import { holds, type Rule, type Values } from './rules'

let counter = 0

const CONTROLS = 'input, select, textarea'

export function linkLabels(root: ParentNode): void {
  for (const label of root.querySelectorAll('label')) {
    const parent = label.parentElement
    if (!parent) continue
    if (!label.id) label.id = `tt-label-${++counter}`
    const controls = [...parent.querySelectorAll<HTMLElement>(CONTROLS)].filter((c) => !c.closest('label'))
    for (const control of controls) {
      if (control.id && label.htmlFor === control.id) continue
      if (control.hasAttribute('aria-label') || control.hasAttribute('aria-labelledby')) continue
      control.setAttribute('aria-labelledby', label.id)
    }
    // A set of chips or checkboxes is one group, named by the label above it.
    const group = parent.querySelector<HTMLElement>('.a2ui-choice-picker')
    if (group && !group.hasAttribute('aria-labelledby') && !group.hasAttribute('aria-label')) {
      group.setAttribute('role', 'group')
      group.setAttribute('aria-labelledby', label.id)
    }
  }
}

// Keeps a screen's inputs named as it changes (a screen opens, an input appears). Returns what
// stops it.
export function keepLabelsLinked(root: HTMLElement): () => void {
  linkLabels(root)
  const observer = new MutationObserver(() => linkLabels(root))
  observer.observe(root, { childList: true, subtree: true })
  return () => observer.disconnect()
}

// What a screen's form calls each field of the app, by the field's name: the label of the input that
// holds it. An input says its field either itself (a location or a signature input names its
// `field`) or through the data it is bound to, which a button's action context hands to the field of
// the same name ({"notes": {"path": "/draft/notes"}}). Only what the person can see counts: the
// inputs of the screen that is open, and not those a Show hides, so an error never names a label
// that isn't on the screen; the first of several inputs for one field is the one named. A field with
// no such input has no label here, and an error about it names the field.
export function fieldLabels(
  components: Iterable<[string, { type?: string; properties: Record<string, unknown> }]>,
  read: (path: string) => unknown,
): Map<string, string> {
  const byId = new Map(components)
  const seen = new Set<string>()
  const visible: { properties: Record<string, unknown> }[] = []
  const visit = (id: unknown) => {
    if (typeof id !== 'string' || seen.has(id)) return
    const c = byId.get(id)
    if (!c) return
    seen.add(id)
    visible.push(c)
    const p = c.properties
    if (c.type === 'Screens') {
      const current = typeof p.current === 'string' ? p.current : read(pathOf(p.current) ?? '')
      const shown = Array.isArray(p.screens)
        ? p.screens.find((s) => (s as { name?: unknown } | null)?.name === current)
        : undefined
      visit((shown as { child?: unknown } | undefined)?.child)
      return
    }
    if (c.type === 'Show') {
      const values = read(`/${p.source === 'record' ? 'record' : 'draft'}`)
      if (holds(p.when as Rule, (values ?? {}) as Values)) visit(p.child)
      return
    }
    visit(p.child)
    if (Array.isArray(p.children)) for (const child of p.children) visit(child)
    else visit((p.children as { componentId?: unknown } | undefined)?.componentId)
    if (Array.isArray(p.tabs)) for (const tab of p.tabs) visit((tab as { child?: unknown } | null)?.child)
    visit(p.trigger)
    visit(p.content)
  }
  // Without the root (the screen isn't whole) nothing is known to be hidden.
  if (byId.has('root')) visit('root')
  else for (const [id] of byId) visit(id)

  const byPath = new Map<string, string>()
  const labels = new Map<string, string>()
  for (const { properties: p } of visible) {
    if (typeof p.label !== 'string' || p.label === '') continue
    const path = pathOf(p.value)
    if (path !== undefined && !byPath.has(path)) byPath.set(path, p.label)
    if (typeof p.field === 'string' && !labels.has(p.field)) labels.set(p.field, p.label)
  }
  for (const { properties: p } of visible) {
    const context = ((p.action as { event?: { context?: unknown } } | undefined)?.event?.context ?? {}) as Record<
      string,
      unknown
    >
    for (const [name, bound] of Object.entries(context)) {
      const path = pathOf(bound)
      const label = path === undefined ? undefined : byPath.get(path)
      if (label && !labels.has(name)) labels.set(name, label)
    }
  }
  return labels
}

function pathOf(bound: unknown): string | undefined {
  const path = (bound as { path?: unknown } | null | undefined)?.path
  return typeof path === 'string' ? path : undefined
}

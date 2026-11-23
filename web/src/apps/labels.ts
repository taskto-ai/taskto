// A2UI's inputs draw a <label> beside their <input> without linking the two, so a screen reader
// reads an unnamed field. This gives each such input the name of the label drawn with it: the
// label and the inputs share a parent (a text field, a date or time input), or the label heads a
// group of choices. It never changes an input that already has a name.

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
// the same name ({"notes": {"path": "/draft/notes"}}). A field with no input on the screen has no
// label here, and an error about it names the field.
export function fieldLabels(
  components: Iterable<[string, { properties: Record<string, unknown> }]>,
): Record<string, string> {
  const byPath = new Map<string, string>()
  const labels: Record<string, string> = {}
  const all = [...components]
  for (const [, { properties: p }] of all) {
    if (typeof p.label !== 'string' || p.label === '') continue
    const path = (p.value as { path?: unknown } | undefined)?.path
    if (typeof path === 'string') byPath.set(path, p.label)
    if (typeof p.field === 'string') labels[p.field] = p.label
  }
  for (const [, { properties: p }] of all) {
    const context = ((p.action as { event?: { context?: unknown } } | undefined)?.event?.context ?? {}) as Record<
      string,
      unknown
    >
    for (const [name, bound] of Object.entries(context)) {
      const path = (bound as { path?: unknown } | null)?.path
      const label = typeof path === 'string' ? byPath.get(path) : undefined
      if (label && !(name in labels)) labels[name] = label
    }
  }
  return labels
}

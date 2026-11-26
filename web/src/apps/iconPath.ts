// An app's icon is SVG path data in its definition, A2UI's {"svgPath": "…"} form of the Icon
// component's name, drawn on a 24 by 24 view box. The server accepts only path data
// (internal/apps/icons.go); this is the same grammar, checked again before drawing, since a
// definition can reach the browser another way than through that check (an offline copy, an
// older release, a value bound from data). Kept apart from the catalog so it can be tested and
// read without loading the renderer.

// The longest path an icon may have (internal/apps MaxIconPathLen).
export const MAX_ICON_PATH = 2000

// A moveto, then only path commands, digits, signs, decimal points, exponents, commas and
// spaces: nothing that could close an attribute, open markup or name a URL, a style or a script.
const PATH_DATA = /^[Mm][MmLlHhVvCcSsQqTtAaZz0-9+\-.eE, ]*$/

// The path to draw for an Icon's name, or null: a name (which this client has no font to draw), a
// bound value that isn't {svgPath}, and a path outside the grammar all draw nothing.
export function iconPath(name: unknown): string | null {
  if (typeof name !== 'object' || name === null || Array.isArray(name)) return null
  const keys = Object.keys(name)
  if (keys.length !== 1 || keys[0] !== 'svgPath') return null
  const d = (name as { svgPath: unknown }).svgPath
  return typeof d === 'string' && d.length <= MAX_ICON_PATH && PATH_DATA.test(d) ? d : null
}

// A component of a surface as the definition gives it: its kind and its props, unresolved.
export type SurfaceComponent = { component: string; props: Record<string, unknown> }

// How deep a control's content is followed through its containers (internal/apps maxIconContentDepth).
const MAX_CONTENT_DEPTH = 4

// The icons given by a name that show their name as text: those in a control (a Button's child, a
// Modal's trigger, followed through Row, Column and Card) that nothing else names. Before icons
// were paths a name was drawn as the word, and a control whose only content it was kept its size
// and its name; it still does. Elsewhere a name draws nothing. The server's label rule walks the
// same way (internal/apps/icons.go).
export function iconsShowingTheirName(components: ReadonlyMap<string, SurfaceComponent>): Set<string> {
  const memo = new Map<string, { named: boolean; names: string[] }>()
  const walk = (id: unknown, depth: number): { named: boolean; names: string[] } => {
    const key = `${depth}:${String(id)}`
    const seen = memo.get(key)
    if (seen) return seen
    let got: { named: boolean; names: string[] } = { named: true, names: [] }
    const c = typeof id === 'string' ? components.get(id) : undefined
    if (c && depth <= MAX_CONTENT_DEPTH) {
      const p = c.props
      switch (c.component) {
        case 'Icon':
          if (typeof p.name === 'string') got = { named: false, names: [id as string] }
          else got = { named: iconPath(p.name) !== null && hasLabel(p.accessibility), names: [] }
          break
        case 'Text':
          if (typeof p.text === 'string' && p.text.trim() === '') got = { named: false, names: [] }
          break
        case 'Divider':
          got = { named: false, names: [] }
          break
        case 'Card':
          got = walk(p.child, depth + 1)
          break
        case 'Row':
        case 'Column':
          if (Array.isArray(p.children)) {
            // Each child once (a fan-out of references to one child costs no more than one), and
            // each name once, so what is kept is bounded by the distinct icons.
            const under = [...new Set(p.children)].map((ch) => walk(ch, depth + 1))
            got = { named: under.some((u) => u.named), names: [...new Set(under.flatMap((u) => u.names))] }
          }
          break
      }
    }
    memo.set(key, got)
    return got
  }
  const showing = new Set<string>()
  for (const c of components.values()) {
    const content = c.component === 'Button' ? c.props.child : c.component === 'Modal' ? c.props.trigger : undefined
    if (content === undefined) continue
    const got = walk(content, 0)
    if (!got.named) for (const id of got.names) showing.add(id)
  }
  return showing
}

function hasLabel(accessibility: unknown): boolean {
  const label = (accessibility as { label?: unknown } | undefined)?.label
  return typeof label === 'string' ? label.trim() !== '' : typeof label === 'object' && label !== null
}

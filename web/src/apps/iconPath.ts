export const MAX_ICON_PATH = 2000

const PATH_DATA = /^[Mm][MmLlHhVvCcSsQqTtAaZz0-9+\-.eE, ]*$/

export function iconPath(name: unknown): string | null {
  if (typeof name !== 'object' || name === null || Array.isArray(name)) return null
  const keys = Object.keys(name)
  if (keys.length !== 1 || keys[0] !== 'svgPath') return null
  const d = (name as { svgPath: unknown }).svgPath
  return typeof d === 'string' && d.length <= MAX_ICON_PATH && PATH_DATA.test(d) ? d : null
}

export type SurfaceComponent = { component: string; props: Record<string, unknown> }

const MAX_CONTENT_DEPTH = 4

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

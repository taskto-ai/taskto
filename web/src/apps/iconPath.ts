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

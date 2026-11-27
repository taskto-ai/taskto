export interface Box {
  top: number
  left: number
  width: number
  height: number
}

export interface PlaceOptions {
  dir: 'ltr' | 'rtl'
  align: 'start' | 'end'
  gap: number
  margin: number
}

export interface Placement {
  top: number
  left: number
  side: 'below' | 'above'
  maxHeight: number
}

const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(v, hi))

export function place(
  anchor: Box,
  box: { width: number; height: number },
  view: { width: number; height: number },
  { dir, align, gap, margin }: PlaceOptions,
): Placement {
  const fromLeft = (align === 'start') === (dir === 'ltr')
  const left = clamp(
    fromLeft ? anchor.left : anchor.left + anchor.width - box.width,
    margin,
    Math.max(margin, view.width - margin - box.width),
  )

  const below = view.height - (anchor.top + anchor.height + gap) - margin
  const above = anchor.top - gap - margin
  const side = box.height <= below || below >= above ? 'below' : 'above'
  const room = Math.max(0, Math.min(side === 'below' ? below : above, view.height - 2 * margin))
  const height = Math.min(box.height, room)
  const top = side === 'below' ? anchor.top + anchor.height + gap : anchor.top - gap - height
  return {
    top: clamp(top, margin, Math.max(margin, view.height - margin - height)),
    left,
    side,
    maxHeight: room,
  }
}

export function scrolledAway(el: Element): boolean {
  const r = el.getBoundingClientRect()
  const outsideX = (b: { left: number; right: number }) => r.right <= b.left || r.left >= b.right
  const outsideY = (b: { top: number; bottom: number }) => r.bottom <= b.top || r.top >= b.bottom
  if (outsideX({ left: 0, right: window.innerWidth }) || outsideY({ top: 0, bottom: window.innerHeight })) return true
  const seen = {
    left: Math.max(r.left, 0),
    right: Math.min(r.right, window.innerWidth),
    top: Math.max(r.top, 0),
    bottom: Math.min(r.bottom, window.innerHeight),
  }
  for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
    const s = getComputedStyle(p)
    const b = p.getBoundingClientRect()
    if (s.overflowX !== 'visible') {
      seen.left = Math.max(seen.left, b.left)
      seen.right = Math.min(seen.right, b.right)
    }
    if (s.overflowY !== 'visible') {
      seen.top = Math.max(seen.top, b.top)
      seen.bottom = Math.min(seen.bottom, b.bottom)
    }
    if (seen.right <= seen.left || seen.bottom <= seen.top) return true
  }
  return false
}

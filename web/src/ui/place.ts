// Where a floating box (a menu) goes beside the thing that opened it: below it, or above when
// there is more room there; aligned to the anchor's inline start or end, which follows the
// writing direction; and always inside the window, a margin from its edges. Pure, so it is
// tested on numbers; usePlacement in Menu.tsx feeds it the real sizes.

export interface Box {
  top: number
  left: number
  width: number
  height: number
}

export interface PlaceOptions {
  dir: 'ltr' | 'rtl'
  // The anchor's edge the box lines up with, in the writing direction.
  align: 'start' | 'end'
  // Between the anchor and the box.
  gap: number
  // Between the box and the window's edges.
  margin: number
}

export interface Placement {
  top: number
  left: number
  side: 'below' | 'above'
  // The most the box may be tall where it is, so a long menu scrolls inside the window.
  maxHeight: number
}

const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(v, hi))

export function place(
  anchor: Box,
  box: { width: number; height: number },
  view: { width: number; height: number },
  { dir, align, gap, margin }: PlaceOptions,
): Placement {
  // The left edge: the anchor's left when the box starts where the anchor starts in a
  // left-to-right page (or ends where it ends in a right-to-left one).
  const fromLeft = (align === 'start') === (dir === 'ltr')
  const left = clamp(
    fromLeft ? anchor.left : anchor.left + anchor.width - box.width,
    margin,
    Math.max(margin, view.width - margin - box.width),
  )

  const below = view.height - (anchor.top + anchor.height + gap) - margin
  const above = anchor.top - gap - margin
  const side = box.height <= below || below >= above ? 'below' : 'above'
  const room = Math.max(0, side === 'below' ? below : above)
  const height = Math.min(box.height, room)
  const top = side === 'below' ? anchor.top + anchor.height + gap : anchor.top - gap - height
  return {
    top: clamp(top, margin, Math.max(margin, view.height - margin - height)),
    left,
    side,
    maxHeight: room,
  }
}

// Whether an element is out of sight in a box that scrolls or clips it, or in the window.
export function scrolledAway(el: Element): boolean {
  const r = el.getBoundingClientRect()
  const outsideX = (b: { left: number; right: number }) => r.right <= b.left || r.left >= b.right
  const outsideY = (b: { top: number; bottom: number }) => r.bottom <= b.top || r.top >= b.bottom
  if (outsideX({ left: 0, right: window.innerWidth }) || outsideY({ top: 0, bottom: window.innerHeight })) return true
  // A box clips on each axis on its own: `overflow-x: clip` with `overflow-y: visible` hides only
  // what is beside it, not what is below it.
  for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
    const s = getComputedStyle(p)
    const b = p.getBoundingClientRect()
    if (s.overflowX !== 'visible' && outsideX(b)) return true
    if (s.overflowY !== 'visible' && outsideY(b)) return true
  }
  return false
}

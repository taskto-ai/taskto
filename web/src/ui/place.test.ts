import { describe, expect, test } from 'vitest'
import { place } from './place'

const view = { width: 1000, height: 800 }
const box = { width: 200, height: 150 }
// A 32px button in the middle of the window.
const anchor = { top: 300, left: 500, width: 32, height: 32 }
const ltr = { dir: 'ltr', align: 'end', gap: 4, margin: 8 } as const

describe('place', () => {
  test('below the anchor, its end edge on the anchor’s end in left-to-right', () => {
    expect(place(anchor, box, view, ltr)).toEqual({ top: 336, left: 332, side: 'below', maxHeight: 456 })
  })

  test('the end is the left in right-to-left, and the start the right', () => {
    expect(place(anchor, box, view, { ...ltr, dir: 'rtl' }).left).toBe(500)
    expect(place(anchor, box, view, { ...ltr, dir: 'rtl', align: 'start' }).left).toBe(332)
    expect(place(anchor, box, view, { ...ltr, align: 'start' }).left).toBe(500)
  })

  test('never taller than the window between its margins, even for an anchor scrolled past its edge', () => {
    // The anchor is below the window: all of the window is "above" it, and more.
    const p = place({ ...anchor, top: 1200 }, { width: 200, height: 900 }, view, ltr)
    expect(p.side).toBe('above')
    expect(p.maxHeight).toBe(800 - 2 * 8)
    expect(p.top).toBe(8)
    // And above the window: all of it is "below".
    const q = place({ ...anchor, top: -400 }, { width: 200, height: 900 }, view, ltr)
    expect(q.maxHeight).toBe(800 - 2 * 8)
  })

  test('above when there is not room below and more above', () => {
    const low = { ...anchor, top: 700 }
    expect(place(low, box, view, ltr)).toMatchObject({ side: 'above', top: 546 })
  })

  test('stays below when neither side has room but below has more, and says how tall it may be', () => {
    const p = place({ ...anchor, top: 100 }, { width: 200, height: 900 }, view, ltr)
    expect(p.side).toBe('below')
    expect(p.maxHeight).toBe(800 - 136 - 8)
    expect(p.top).toBe(136)
  })

  test('kept inside the window near an edge', () => {
    // A button at the left edge of a right-to-left page: the box would start off the window.
    expect(place({ ...anchor, left: 0 }, box, view, { ...ltr, dir: 'ltr' }).left).toBe(8)
    // At the right edge, aligned to its start: pushed back in.
    expect(place({ ...anchor, left: 980, width: 20 }, box, view, { ...ltr, align: 'start' }).left).toBe(792)
    // Wider than the window: from the margin.
    expect(place(anchor, { width: 1200, height: 100 }, view, ltr).left).toBe(8)
  })
})

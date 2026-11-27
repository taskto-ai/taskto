// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest'
import { scrolledAway } from './place'

afterEach(() => {
  vi.restoreAllMocks()
  document.body.innerHTML = ''
})

const rect = (top: number, left: number, height: number, width: number) =>
  ({ top, left, bottom: top + height, right: left + width, width, height, x: left, y: top, toJSON() {} }) as DOMRect

function setup(overflowX: string, overflowY: string, button: DOMRect) {
  document.body.innerHTML = `<div id="box" style="overflow-x: ${overflowX}; overflow-y: ${overflowY}"><button id="b">x</button></div>`
  const box = document.getElementById('box') as HTMLElement
  const el = document.getElementById('b') as HTMLElement
  vi.spyOn(box, 'getBoundingClientRect').mockReturnValue(rect(100, 100, 100, 300))
  vi.spyOn(el, 'getBoundingClientRect').mockReturnValue(button)
  return el
}

test('a box that clips only sideways hides only what is beside it', () => {
  expect(scrolledAway(setup('clip', 'visible', rect(220, 120, 32, 32)))).toBe(false)
  expect(scrolledAway(setup('clip', 'visible', rect(120, 500, 32, 32)))).toBe(true)
})

test('a box that scrolls up and down hides what is above or below it', () => {
  expect(scrolledAway(setup('visible', 'auto', rect(220, 120, 32, 32)))).toBe(true)
  expect(scrolledAway(setup('visible', 'auto', rect(120, 120, 32, 32)))).toBe(false)
})

test('out of the window is out of sight', () => {
  expect(scrolledAway(setup('visible', 'visible', rect(-100, 120, 32, 32)))).toBe(true)
})

test('two boxes that each show a part of it, but not the same part, hide it', () => {
  document.body.innerHTML =
    '<div id="outer" style="overflow-y: auto"><div id="inner" style="overflow-y: auto"><button id="b">x</button></div></div>'
  const el = document.getElementById('b') as HTMLElement
  vi.spyOn(el, 'getBoundingClientRect').mockReturnValue(rect(100, 100, 100, 32))
  vi.spyOn(document.getElementById('inner') as HTMLElement, 'getBoundingClientRect').mockReturnValue(
    rect(100, 0, 40, 500),
  )
  vi.spyOn(document.getElementById('outer') as HTMLElement, 'getBoundingClientRect').mockReturnValue(
    rect(160, 0, 40, 500),
  )
  expect(scrolledAway(el)).toBe(true)
})

test('a box that shows only a part of it outside the window hides it', () => {
  document.body.innerHTML = '<div id="box" style="overflow-y: auto"><button id="b">x</button></div>'
  const el = document.getElementById('b') as HTMLElement
  vi.spyOn(el, 'getBoundingClientRect').mockReturnValue(rect(-100, 100, 200, 32))
  vi.spyOn(document.getElementById('box') as HTMLElement, 'getBoundingClientRect').mockReturnValue(
    rect(-100, 0, 80, 500),
  )
  expect(scrolledAway(el)).toBe(true)
})

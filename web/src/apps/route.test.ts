// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest'
import { appItem, openRunWindow, parseRunPath, runPath } from './route'

afterEach(() => {
  vi.unstubAllGlobals()
})

test('a run path is the app item under the workspace, with or without a screen', () => {
  expect(runPath(1, 3, 7)).toBe('/w/1/run/3-7')
  expect(runPath(1, 3, 7, 'stock')).toBe('/w/1/run/3-7-stock')
})

test('every item an app link makes is read back from its run path', () => {
  for (const screen of [undefined, 'home', 'stock_count', 'a1']) {
    expect(parseRunPath(runPath(12, 3, 7, screen))).toEqual({
      workspace: 12,
      space: 3,
      app: 7,
      ...(screen ? { screen } : {}),
    })
    expect(runPath(12, 3, 7, screen)).toBe(`/w/12/run/${appItem(3, 7, screen)}`)
  }
  expect(parseRunPath('/w/1/run/3-7/')).toEqual({ workspace: 1, space: 3, app: 7 })
})

test('a path that is not an app item is not a run path', () => {
  for (const path of [
    '/w/1/run/x',
    '/w/1/run/3',
    '/w/1/run/3-',
    '/w/1/run/3-7-Bad',
    '/w/1/run/3-7-9x',
    '/w/1/run/3-7-a-b',
    '/w/1/run/',
    '/w/1/run',
    '/w/x/run/3-7',
    '/w/1/apps/3-7',
    '/w/1/run/3-7/extra',
    '/run/3-7',
  ]) {
    expect(parseRunPath(path), path).toBeNull()
  }
})

function fakeWindow() {
  const order: string[] = []
  let opener: unknown = { here: true }
  let href = ''
  const popup = {
    closed: false,
    focus: () => order.push('focus'),
    get opener() {
      return opener
    },
    set opener(value) {
      opener = value
      order.push(`opener:${String(value)}`)
    },
    location: {
      get href() {
        return href
      },
      set href(value: string) {
        href = value
        order.push(`go:${value}`)
      },
    },
  }
  return { popup: popup as unknown as Window, order, opener: () => opener }
}

test('the window is a blank popup named for the app that lets go of its opener before it goes to the run path', () => {
  const made = fakeWindow()
  const open = vi.fn(() => made.popup)
  vi.stubGlobal('open', open)
  openRunWindow(1, 3, 17, 'stock')
  expect(open).toHaveBeenCalledWith('', 'taskto-app-3-17', 'popup,width=480,height=800')
  expect(made.opener()).toBeNull()
  expect(made.order).toEqual(['opener:null', 'go:/w/1/run/3-17-stock', 'focus'])
})

test('a second click goes to the same popup, and a closed popup is opened again', () => {
  const first = fakeWindow()
  const second = fakeWindow()
  const open = vi.fn().mockReturnValueOnce(first.popup).mockReturnValueOnce(second.popup)
  vi.stubGlobal('open', open)
  openRunWindow(1, 3, 18)
  openRunWindow(1, 3, 18, 'home')
  expect(open).toHaveBeenCalledTimes(1)
  expect(first.order).toEqual([
    'opener:null',
    'go:/w/1/run/3-18',
    'focus',
    'opener:null',
    'go:/w/1/run/3-18-home',
    'focus',
  ])
  ;(first.popup as { closed: boolean }).closed = true
  openRunWindow(1, 3, 18)
  expect(open).toHaveBeenCalledTimes(2)
  expect(second.order).toEqual(['opener:null', 'go:/w/1/run/3-18', 'focus'])
})

test('a popup the browser refuses leaves nothing to reuse and breaks nothing', () => {
  const open = vi.fn().mockReturnValueOnce(null)
  vi.stubGlobal('open', open)
  expect(() => openRunWindow(1, 3, 19)).not.toThrow()
  const made = fakeWindow()
  open.mockReturnValueOnce(made.popup)
  openRunWindow(1, 3, 19)
  expect(open).toHaveBeenCalledTimes(2)
  expect(made.order).toEqual(['opener:null', 'go:/w/1/run/3-19', 'focus'])
})

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

test('the window opens at the run path, named for the app, as a popup', () => {
  const open = vi.fn()
  vi.stubGlobal('open', open)
  openRunWindow(1, 3, 7, 'stock')
  openRunWindow(1, 3, 7)
  expect(open).toHaveBeenNthCalledWith(1, '/w/1/run/3-7-stock', 'taskto-app-3-7', 'popup,width=480,height=800')
  expect(open).toHaveBeenNthCalledWith(2, '/w/1/run/3-7', 'taskto-app-3-7', 'popup,width=480,height=800')
})

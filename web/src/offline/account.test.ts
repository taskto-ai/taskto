// @vitest-environment jsdom
//
// What a tab hears of the browser's account from the other tabs: each change as it was made, in
// order, even when the tab handles them late (it was frozen, in the background).

import { afterEach, expect, test } from 'vitest'
import { onAccountSignal, watchAccount } from './account'

const LAST = 'taskto:offline-last'
const off: (() => void)[] = []

afterEach(() => {
  for (const fn of off.splice(0)) fn()
  localStorage.clear()
})

function heard(): (number | null)[] {
  watchAccount()
  const got: (number | null)[] = []
  off.push(onAccountSignal((who) => got.push(who)))
  return got
}

const marker = (id: number, name: string) => JSON.stringify({ id, name })

test('a sign-out and the next sign-in, handled late, are heard as each was: nobody, then the other account', () => {
  const got = heard()
  // By the time this tab handles them, storage already names the second person.
  localStorage.setItem(LAST, marker(2, 'Second'))
  window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: null }))
  window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: marker(2, 'Second') }))
  expect(got).toEqual([null, 2])
})

test('storage cleared altogether is nobody; a value that is no account is nobody too', () => {
  const got = heard()
  localStorage.setItem(LAST, marker(2, 'Second'))
  window.dispatchEvent(new StorageEvent('storage', { key: null, newValue: null }))
  window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: '{"id":"x"}' }))
  window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: 'not json' }))
  expect(got).toEqual([null, null, null])
})

test('while offline starts are refused here, every change is heard as nobody', () => {
  const got = heard()
  localStorage.setItem('taskto:offline-refused', '1')
  window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: marker(2, 'Second') }))
  expect(got).toEqual([null])
})

test('a change of another key is not a change of account', () => {
  const got = heard()
  window.dispatchEvent(new StorageEvent('storage', { key: 'taskto:drafts-of', newValue: '2' }))
  expect(got).toEqual([])
})

// @vitest-environment jsdom

import { cleanup, render } from '@testing-library/react'
import { afterEach, describe, expect, test } from 'vitest'
import { Avatar, initials, initialsAmong } from './Avatar'

afterEach(cleanup)

describe('initialsAmong', () => {
  test('names that start alike are told apart by their letters, not by hue alone', () => {
    expect(initials('Equipment')).toBe(initials('Expenses'))
    expect(initialsAmong(['Equipment', 'Expenses'])).toEqual(['EQ', 'EX'])
    expect(initialsAmong(['E\u0301tudes'])).toEqual(['E\u0301T'])
  })

  test('a name of several words keeps its usual initials', () => {
    expect(initialsAmong(['Field visits', 'Expenses'])).toEqual(['FV', 'EX'])
  })

  test('names whose first two letters match take a later letter, each one its own', () => {
    const letters = initialsAmong(['Expenses', 'Export', 'Expo'])
    expect(new Set(letters).size).toBe(3)
    for (const l of letters) expect(l.startsWith('E')).toBe(true)
  })

  test("a short name's only letters stay its own, in either order", () => {
    expect(initialsAmong(['Exports', 'Ex'])).toEqual(['EP', 'EX'])
    expect(initialsAmong(['Ex', 'Exports'])).toEqual(['EX', 'EP'])
  })

  test('whole characters, never half of one', () => {
    expect(initialsAmong(['Égalité', 'Ösel'])).toEqual(['ÉG', 'ÖS'])
  })
})

test('an avatar shows the letters it is given in place of its initials', () => {
  const { container } = render(<Avatar name="Equipment" letters="EQ" decorative />)
  expect(container.textContent).toBe('EQ')
})

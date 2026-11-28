import { describe, expect, it } from 'vitest'
import { pastDeadline } from './revision.js'

describe('pastDeadline', () => {
  it('has no deadline to pass when none was sent', () => {
    expect(pastDeadline(undefined, 10)).toBe(false)
  })

  it('is false up to and including the admit-by time', () => {
    expect(pastDeadline(100, 99)).toBe(false)
    expect(pastDeadline(100, 100)).toBe(false)
  })

  it('is true once the admit-by time has passed', () => {
    expect(pastDeadline(100, 101)).toBe(true)
  })
})

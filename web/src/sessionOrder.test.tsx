// @vitest-environment jsdom

import { render, waitFor } from '@testing-library/react'
import { useEffect, useLayoutEffect } from 'react'
import { expect, test, vi } from 'vitest'
import { sameSession, sessionUser } from './session'
import { fakeFetch } from './test/fakes'

const seen = vi.hoisted(() => ({
  layout: [] as (number | null)[],
  passive: [] as (number | null)[],
  same: [] as (() => boolean)[],
}))

vi.mock('./shell/Shell', () => ({
  Shell: () => {
    useLayoutEffect(() => {
      seen.layout.push(sessionUser())
      seen.same.push(sameSession())
    }, [])
    useEffect(() => {
      seen.passive.push(sessionUser())
    }, [])
    return null
  },
}))

const { App } = await import('./App')

test("the shell's first effects already run in the session of the person who signed in", async () => {
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { user: { id: 1, email: 'owner@example.com', name: 'Owner' }, workspaces: [] } },
  })
  render(<App />)
  await waitFor(() => expect(seen.passive).toHaveLength(1))
  expect(seen.layout).toEqual([1])
  expect(seen.passive).toEqual([1])
  await new Promise((r) => setTimeout(r, 20))
  expect(seen.same[0]()).toBe(true)
})

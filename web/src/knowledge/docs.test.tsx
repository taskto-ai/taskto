// @vitest-environment jsdom

import { act, cleanup, renderHook, waitFor } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { fakeFetch } from '../test/fakes'
import { useDocs } from './docs'
import { useDraft } from './useDraft'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const S = '/api/workspaces/1/spaces/3'
const V1 = 'a'.repeat(40)
const doc = (path: string) => ({ path, version: V1, size: 1, versioned: true })

test('the list is every page of it, and nothing is asked without a space', async () => {
  const calls = fakeFetch({
    [`GET ${S}/docs?limit=1000`]: { body: { docs: [doc('a.md')], next: 'a.md' } },
    [`GET ${S}/docs?limit=1000&after=a.md`]: { body: { docs: [doc('b c.md')], next: 'b c.md' } },
    [`GET ${S}/docs?limit=1000&after=b%20c.md`]: { body: { docs: [doc('d.md')] } },
  })
  const none = renderHook(() => useDocs(null))
  await waitFor(() => expect(none.result.current.error).toBeNull())
  expect(none.result.current.data).toBeUndefined()
  expect(calls).toHaveLength(0)

  const { result } = renderHook(() => useDocs(S))
  await waitFor(() => expect(result.current.data?.map((d) => d.path)).toEqual(['a.md', 'b c.md', 'd.md']))
  expect(calls.map((c) => c.path)).toEqual([
    `${S}/docs?limit=1000`,
    `${S}/docs?limit=1000&after=a.md`,
    `${S}/docs?limit=1000&after=b%20c.md`,
  ])
})

test('a draft whose document went away is saved again only when asked, as a new document', async () => {
  const url = `${S}/docs/plan.md`
  let gone = true
  const calls = fakeFetch({
    [`PUT ${url}`]: () => (gone ? { status: 409, body: { error: 'conflict' } } : { body: { commit: 'c2' } }),
    [`GET ${url}`]: { status: 404, body: { error: 'not found' } },
  })
  const onSaved = vi.fn()
  const { result } = renderHook(() => useDraft(url, { ...doc('plan.md'), content: '# Plan\n' }, onSaved))
  act(() => result.current.setText('Mine.'))
  await act(() => result.current.save())
  expect(result.current.conflict).toEqual({ current: null })
  expect(onSaved).not.toHaveBeenCalled()

  gone = false
  await act(async () => result.current.overwrite(null))
  await waitFor(() => expect(onSaved).toHaveBeenCalledOnce())
  expect(calls.filter((c) => c.method === 'PUT').map((c) => c.body)).toEqual([
    { content: 'Mine.', base_version: V1 },
    { content: 'Mine.' },
  ])
})

// @vitest-environment jsdom

import { act, renderHook, waitFor } from '@testing-library/react'
import { expect, test } from 'vitest'
import { useLoad } from './load'

test('a failed reload keeps what is shown, under the error', async () => {
  let fail = false
  const { result } = renderHook(() =>
    useLoad(() => (fail ? Promise.reject(new Error('offline')) : Promise.resolve(['a'])), []),
  )
  await waitFor(() => expect(result.current.data).toEqual(['a']))
  fail = true
  act(() => result.current.reload())
  await waitFor(() => expect(result.current.error).toBe('offline'))
  expect(result.current.data).toEqual(['a'])
})

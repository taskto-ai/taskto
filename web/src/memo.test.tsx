// @vitest-environment jsdom

import { cleanup, render, renderHook, screen } from '@testing-library/react'
import { type ReactNode, StrictMode, useState } from 'react'
import { afterEach, expect, test } from 'vitest'
import { useChanged, useMemoBy } from './memo'

afterEach(cleanup)

test('keeps the value while the deps are the same, and makes a new one when one changes', () => {
  let made = 0
  const { result, rerender } = renderHook(({ a, b }) => useMemoBy(() => ({ made: ++made, a, b }), [a, b]), {
    initialProps: { a: 1, b: 'x' },
  })
  const first = result.current
  rerender({ a: 1, b: 'x' })
  expect(result.current).toBe(first)
  rerender({ a: 2, b: 'x' })
  expect(result.current).not.toBe(first)
  expect(result.current).toMatchObject({ a: 2, b: 'x' })
  const second = result.current
  rerender({ a: 2, b: 'x' })
  expect(result.current).toBe(second)
})

function Counter({ value }: { value: string }) {
  const [changes, setChanges] = useState(0)
  const changed = useChanged(value)
  if (changed) setChanges((n) => n + 1)
  return <p>{changes}</p>
}

test.each([
  ['', (c: ReactNode) => <>{c}</>],
  [' under StrictMode', (c: ReactNode) => <StrictMode>{c}</StrictMode>],
])('says once that a value changed, never for the first render%s', (_, wrap) => {
  const { rerender } = render(wrap(<Counter value="a" />))
  expect(screen.getByText('0')).toBeTruthy()
  rerender(wrap(<Counter value="a" />))
  expect(screen.getByText('0')).toBeTruthy()
  rerender(wrap(<Counter value="b" />))
  expect(screen.getByText('1')).toBeTruthy()
  rerender(wrap(<Counter value="b" />))
  expect(screen.getByText('1')).toBeTruthy()
  rerender(wrap(<Counter value="c" />))
  expect(screen.getByText('2')).toBeTruthy()
})

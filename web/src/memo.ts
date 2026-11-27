import { type DependencyList, useState } from 'react'

function sameDeps(a: DependencyList, b: DependencyList): boolean {
  return a.length === b.length && a.every((v, i) => Object.is(v, b[i]))
}

export function useMemoBy<T>(make: () => T, deps: DependencyList): T {
  const [memo, setMemo] = useState(() => ({ value: make(), deps }))
  if (sameDeps(memo.deps, deps)) return memo.value
  const next = { value: make(), deps }
  setMemo(next)
  return next.value
}

export function useChanged(value: unknown): boolean {
  const [seen, setSeen] = useState(value)
  if (Object.is(seen, value)) return false
  setSeen(value)
  return true
}

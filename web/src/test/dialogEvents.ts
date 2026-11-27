interface Closes {
  test: number
  pending: number
  waiters: (() => void)[]
}
const key = Symbol.for('taskto.test.dialogCloses')
const g = globalThis as typeof globalThis & { [key]?: Closes }
const state: Closes = g[key] ?? { test: 0, pending: 0, waiters: [] }
g[key] = state

export function closeQueued(): () => void {
  const test = state.test
  state.pending++
  return () => {
    if (test !== state.test) return
    state.pending--
    if (state.pending > 0) return
    const done = state.waiters
    state.waiters = []
    for (const resolve of done) resolve()
  }
}

export function closesDelivered(): Promise<void> {
  if (state.pending === 0) return Promise.resolve()
  return new Promise((resolve) => state.waiters.push(resolve))
}

export function resetCloses(): void {
  state.test++
  state.pending = 0
  state.waiters = []
}

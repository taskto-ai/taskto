// The close events the test setup's <dialog> stands-in has queued (a browser fires close in a task
// of its own), so a test can wait until they have all arrived, not for a guessed number of
// tasks, which a loaded machine can outrun.
//
// Each test counts only its own: a close queued by the last test's cleanup (a dialog unmounted
// open) arrives during the next one, and must not answer that test's wait.

// Kept on the global object: the setup file and a test file may each load their own copy of
// this module, and they must count the same events.
interface Closes {
  test: number
  pending: number
  waiters: (() => void)[]
}
const key = Symbol.for('taskto.test.dialogCloses')
const g = globalThis as typeof globalThis & { [key]?: Closes }
const state: Closes = g[key] ?? { test: 0, pending: 0, waiters: [] }
g[key] = state

// Called when a close is queued; the function it returns, when that close has been dispatched.
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

// Resolves once every close event this test has queued so far has been dispatched (and its
// listeners have run).
export function closesDelivered(): Promise<void> {
  if (state.pending === 0) return Promise.resolve()
  return new Promise((resolve) => state.waiters.push(resolve))
}

// A new test starts with nothing queued.
export function resetCloses(): void {
  state.test++
  state.pending = 0
  state.waiters = []
}

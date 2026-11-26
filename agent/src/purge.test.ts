import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { type PurgeJob, purgeOffThread } from './purge.js'
import { WalBusyError } from './wal.js'

const job: PurgeJob = { op: 'truncate', dbPath: '/nonexistent/x.db', walBusyMs: 10 }

// How the thread's answer becomes the promise's: the sidecar's route turns a WalBusyError into the
// 503 that says the purge isn't done, and anything else into a 500.
describe('purgeOffThread', () => {
  let dir: string
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'taskto-purge-'))
  })
  afterEach(() => rmSync(dir, { recursive: true, force: true }))

  const worker = (name: string, source: string) => {
    const path = join(dir, name)
    writeFileSync(path, source)
    return pathToFileURL(path)
  }

  it('settles when the thread says it is done, and runs off the calling thread', async () => {
    const url = worker(
      'ok.mjs',
      `import { parentPort, threadId } from 'node:worker_threads'
parentPort.postMessage({ ok: true, threadId })`,
    )
    await expect(purgeOffThread(job, url)).resolves.toBeUndefined()
  })

  it('does not block the calling thread while the work runs', async () => {
    const url = worker(
      'slow.mjs',
      `import { parentPort } from 'node:worker_threads'
Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 300)
parentPort.postMessage({ ok: true })`,
    )
    let ticks = 0
    const timer = setInterval(() => ticks++, 10)
    try {
      await purgeOffThread(job, url)
    } finally {
      clearInterval(timer)
    }
    // A thread that slept 300 ms here would have let no timer fire.
    expect(ticks).toBeGreaterThan(5)
  })

  it('rejects with WalBusyError when a reader holds the log', async () => {
    const url = worker(
      'busy.mjs',
      `import { parentPort } from 'node:worker_threads'
parentPort.postMessage({ ok: false, busy: true, message: 'a reader holds the log' })`,
    )
    await expect(purgeOffThread(job, url)).rejects.toBeInstanceOf(WalBusyError)
  })

  it('rejects with the failure, a crash, or a thread that ends without answering', async () => {
    const failed = worker(
      'failed.mjs',
      `import { parentPort } from 'node:worker_threads'
parentPort.postMessage({ ok: false, busy: false, message: 'disk full' })`,
    )
    await expect(purgeOffThread(job, failed)).rejects.toThrow('disk full')
    const crashed = worker('crashed.mjs', `throw new Error('boom')`)
    await expect(purgeOffThread(job, crashed)).rejects.toThrow('boom')
    const silent = worker('silent.mjs', `// ends at once`)
    await expect(purgeOffThread(job, silent)).rejects.toThrow('before it answered')
  })
})

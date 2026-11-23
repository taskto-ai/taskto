// One tab at a time changes existing records offline (docs/offline.md, "One writer").
//
// Edits and deletions of downloaded records, the choices about a change that needs attention, and
// discards are made by the tab that holds the *offline writer*: one per account on this browser.
// The other tabs of the account show the queue and the records and change nothing; they say so,
// and offer to take over. That one rule replaces what several tabs writing one queue would need
// (merging, re-checking, tabs undoing each other's choices).
//
// Two things make it so, and neither alone is enough:
//   - who may write is what the database says: a row in the `meta` store names the writer's
//     identity, and every write only the writer makes (`OfflineStore.assertWriter`) reads that row
//     in its own transaction and is refused unless it names this tab. A tab that lost the right
//     while a write was on its way is refused by the database, not by what it believed.
//   - who holds it, and for how long, is a Web Lock where the browser has them: held for as long as
//     the tab keeps the right, released when the tab closes. Where it hasn't, the row has a lease
//     that its holder renews with a heartbeat, and anyone may take it once it ran out.
// Taking over asks the holder to let go (a message on a BroadcastChannel) and waits for the lock
// (or the lease): the holder stops writing at once, and the waiting tab becomes the writer. Nobody
// is ever made the writer without the person pressing the button, other than the first tab.

import type { OfflineStore } from './store'

export const WRITER_LEASE_MS = 15_000
const POLL_MS = 400

export type WriterDeps = {
  now?: () => number
  // Runs fn every ms until the returned function is called (the lease's heartbeat): a test's own.
  heartbeat?: (fn: () => void, ms: number) => () => void
  // Where the poll of a tab that waits for a lease is: a test's own.
  setTimer?: (fn: () => void, ms: number) => unknown
  clearTimer?: (handle: unknown) => void
  // The browser's Web Locks (null: none), and the channel the tabs of an account hear each other on.
  locks?: LockManager | null
  channel?: (name: string) => Pick<BroadcastChannel, 'postMessage' | 'close' | 'onmessage'> | null
}

const randomId = () => {
  const bytes = new Uint8Array(8)
  globalThis.crypto.getRandomValues(bytes)
  return [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('')
}

export class OfflineWriter {
  readonly owner = randomId()
  private held = false
  private listeners = new Set<() => void>()
  private stopped = false
  // Lets go of the Web Lock this tab holds (the promise that keeps it resolves).
  private unlock: (() => void) | undefined
  private stopBeat: (() => void) | undefined
  private waiting: AbortController | undefined
  private poll: unknown
  private chan: ReturnType<NonNullable<WriterDeps['channel']>> = null
  private readonly now: () => number

  constructor(
    private readonly store: OfflineStore,
    private readonly deps: WriterDeps = {},
  ) {
    this.now = deps.now ?? Date.now
    store.attachWriter(this.owner)
  }

  private get locks(): LockManager | null {
    if (this.deps.locks !== undefined) return this.deps.locks
    return typeof navigator !== 'undefined' && navigator.locks ? navigator.locks : null
  }

  private get name() {
    return `taskto-offline-writer-${this.store.user}`
  }

  // Whether this tab may change existing records now.
  holds(): boolean {
    return this.held && !this.stopped
  }

  subscribe(fn: () => void): () => void {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  private set(held: boolean) {
    if (this.held === held) return
    this.held = held
    for (const fn of [...this.listeners]) fn()
  }

  // Listens for another tab asking this one to let go, and takes the right if it is free.
  async start(): Promise<void> {
    this.stopped = false
    try {
      this.chan = this.deps.channel
        ? this.deps.channel(this.name)
        : typeof BroadcastChannel === 'undefined'
          ? null
          : new BroadcastChannel(this.name)
      if (this.chan) {
        this.chan.onmessage = (e: MessageEvent) => {
          const msg = e.data as { want?: string } | null
          if (msg?.want && msg.want !== this.owner) void this.yieldWriter()
        }
      }
    } catch {
      this.chan = null
    }
    await this.acquire(false)
  }

  // The person presses "make changes here": the holder is asked to let go, and this waits for the
  // right (the lock, or the lease) and resolves once this tab holds it.
  async takeOver(): Promise<void> {
    if (this.holds()) return
    this.waiting?.abort()
    const wait = new AbortController()
    this.waiting = wait
    try {
      this.chan?.postMessage({ want: this.owner })
    } catch {
      // The holder is not told: the lock or the lease ends when it does.
    }
    try {
      await this.acquire(true, wait.signal)
    } finally {
      if (this.waiting === wait) this.waiting = undefined
    }
  }

  // Lets go of the right for good (the tab is done with this account's database).
  stop() {
    this.stopped = true
    this.waiting?.abort()
    this.clearPoll()
    void this.release()
    try {
      this.chan?.close()
    } catch {
      // Closed already.
    }
    this.chan = null
    this.set(false)
  }

  private async acquire(wait: boolean, signal?: AbortSignal): Promise<void> {
    if (this.stopped || this.held) return
    try {
      if (this.locks) await this.acquireLock(wait, signal)
      else await this.acquireLease(wait, signal)
    } catch {
      // The database is closed, or the wait was cancelled: this tab is not the writer.
    }
  }

  // With Web Locks: the lock is held for as long as the promise the callback returns is pending.
  private acquireLock(wait: boolean, signal?: AbortSignal): Promise<void> {
    const locks = this.locks as LockManager
    return new Promise<void>((resolve) => {
      const options: LockOptions = wait ? { signal } : { ifAvailable: true }
      locks
        .request(this.name, options, async (lock) => {
          if (!lock) {
            resolve()
            return
          }
          // Exclusive by the lock: what a tab that is gone left in the row is over-written.
          try {
            await this.store.claimWriter(this.owner, Number.MAX_SAFE_INTEGER, () => true)
          } catch {
            resolve()
            return
          }
          await new Promise<void>((release) => {
            this.unlock = release
            this.set(!this.stopped)
            resolve()
          })
        })
        .catch(() => resolve())
    })
  }

  // Without them: the row's lease, taken when it is free or has run out, and renewed while held.
  private async acquireLease(wait: boolean, signal?: AbortSignal): Promise<void> {
    const tryTake = async () => {
      const at = this.now()
      return this.store.claimWriter(this.owner, at + WRITER_LEASE_MS, (row) => row === undefined || row.until <= at)
    }
    if (await tryTake()) return this.leased()
    if (!wait) return
    await new Promise<void>((resolve) => {
      const again = () => {
        this.poll = undefined
        if (signal?.aborted || this.stopped) return resolve()
        tryTake().then(
          (got) => {
            if (got) {
              this.leased()
              resolve()
            } else this.poll = (this.deps.setTimer ?? setTimeout)(again, POLL_MS)
          },
          () => resolve(),
        )
      }
      signal?.addEventListener('abort', () => {
        this.clearPoll()
        resolve()
      })
      this.poll = (this.deps.setTimer ?? setTimeout)(again, POLL_MS)
    })
  }

  private leased() {
    this.set(!this.stopped)
    this.stopBeat?.()
    this.stopBeat = (this.deps.heartbeat ?? realHeartbeat)(() => {
      const at = this.now()
      this.store
        .claimWriter(this.owner, at + WRITER_LEASE_MS, () => false)
        .then(
          (still) => {
            if (!still) this.lost()
          },
          () => this.lost(),
        )
    }, WRITER_LEASE_MS / 3)
  }

  private clearPoll() {
    if (this.poll !== undefined) (this.deps.clearTimer ?? clearTimeout)(this.poll as never)
    this.poll = undefined
  }

  // The holder is asked to let go: it stops writing at once and stays a reader.
  private async yieldWriter() {
    if (!this.held) return
    this.set(false)
    await this.release()
  }

  // The right was taken from this tab (or the database can't say): it is a reader.
  private lost() {
    this.stopBeat?.()
    this.stopBeat = undefined
    this.set(false)
  }

  private async release() {
    this.stopBeat?.()
    this.stopBeat = undefined
    const unlock = this.unlock
    this.unlock = undefined
    try {
      await this.store.releaseWriter(this.owner)
    } catch {
      // Closed: the row is let go with the database, or by the next holder.
    }
    unlock?.()
  }
}

const realHeartbeat = (fn: () => void, ms: number) => {
  const handle = setInterval(fn, ms)
  return () => clearInterval(handle)
}

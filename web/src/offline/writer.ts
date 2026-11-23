// One tab at a time changes existing records offline (docs/offline.md, "One writer").
//
// Edits and deletions of downloaded records, the choices about a change that needs attention, and
// discards are made by the tab that holds the *offline writer*: one per account on this browser.
// The other tabs of the account show the queue and the records and change nothing; they say so,
// and offer to take over. That one rule replaces what several tabs writing one queue would need
// (merging, re-checking, tabs undoing each other's choices).
//
// One source of truth, and a way to take turns:
//   - who may write is what the database says: a row in the `meta` store names the writer's
//     identity and a lease (`until`) that its holder renews with a heartbeat. The row is taken
//     only by a compare-and-set, in one transaction, when it is free or its lease has run out
//     (`OfflineStore.claimWriter`), and every write only the writer makes
//     (`OfflineStore.assertWriter`) reads that row in its own transaction and is refused unless it
//     names this tab. A tab that lost the right while a write was on its way is refused by the
//     database, not by what it believed.
//   - Web Locks, where the browser has them, only serialize who is trying: a tab takes the lock
//     before it tries the row, so tabs with Web Locks never race one another for it, and a row
//     left by a holder whose lock is gone (its tab was closed) is free to the one that holds the
//     lock. They never decide on their own: a tab without Web Locks, or one that holds the row by
//     the lease alone, is respected the same way, because the row says so.
// Taking over asks the holder to let go (a message on a BroadcastChannel) and waits until the row
// is free: the holder stops writing at once, and the waiting tab becomes the writer. Nobody is
// ever made the writer without the person pressing the button, other than the first tab.

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
  // Counts the times this tab became the writer: what a screen drawn before a hand-over (to another
  // tab and back, or from a reader to the writer) is told apart by (`session`).
  private epoch = 0
  private listeners = new Set<() => void>()
  private stopped = false
  // Lets go of the Web Lock this tab holds (the promise that keeps it resolves).
  private unlock: (() => void) | undefined
  private stopBeat: (() => void) | undefined
  // The person's take-over that is waiting, and the acquisition that is being made (a start's or a
  // take-over's): stopping aborts them, and one that was granted after that lets go of what it got.
  private waiting: AbortController | undefined
  private acquiring: AbortController | undefined
  // Acquisitions are made one after the other, never at once: two of one tab would race for its
  // own row.
  private turn: Promise<void> = Promise.resolve()
  private poll: unknown
  private chan: ReturnType<NonNullable<WriterDeps['channel']>> = null
  private readonly now: () => number

  constructor(
    private readonly store: OfflineStore,
    private readonly deps: WriterDeps = {},
  ) {
    this.now = deps.now ?? Date.now
    store.attachWriter(this.owner, this.now)
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

  // Which time this tab became the writer, or 0 while it is not: a screen drawn in one session is
  // not the screen of another, since the queue may have been changed by the writer in between
  // (transport.ts: a press is bound to the session its screen was drawn in).
  session(): number {
    return this.holds() ? this.epoch : 0
  }

  subscribe(fn: () => void): () => void {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  private set(held: boolean) {
    if (this.held === held) return
    this.held = held
    if (held) this.epoch++
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
    await this.acquire(false, new AbortController())
  }

  // The person presses "make changes here": the holder is asked to let go, and this waits for the
  // right (the row is free, and with Web Locks, the lock is this tab's) and resolves once this tab
  // holds it.
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
      await this.acquire(true, wait)
    } finally {
      if (this.waiting === wait) this.waiting = undefined
    }
  }

  // Lets go of the right for good (the tab is done with this account's database).
  stop() {
    this.stopped = true
    this.waiting?.abort()
    this.acquiring?.abort()
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

  private acquire(wait: boolean, ctl: AbortController): Promise<void> {
    const mine = this.turn.then(() => this.acquireNow(wait, ctl))
    this.turn = mine
    return mine
  }

  private async acquireNow(wait: boolean, ctl: AbortController): Promise<void> {
    if (this.stopped || this.held || ctl.signal.aborted) return
    this.acquiring = ctl
    try {
      if (this.locks) await this.acquireLock(wait, ctl.signal)
      else await this.acquireLease(wait, ctl.signal)
    } catch {
      // The database is closed, or the wait was cancelled: this tab is not the writer.
    } finally {
      if (this.acquiring === ctl) this.acquiring = undefined
    }
  }

  // One compare-and-set on the writer row, repeated every POLL_MS while `wait`: whether this tab
  // holds it. Free is a row nobody holds, or one whose lease has run out; with the lock in hand
  // (`via: 'lock'`) also one a holder with a lock left (its lock is gone: this one is the only tab
  // with a lock that can be here). A row a tab without Web Locks holds by its lease is never taken
  // from it before the lease has run out, whatever lock this tab holds.
  private async takeRow(wait: boolean, signal: AbortSignal, via: 'lock' | 'lease'): Promise<boolean> {
    for (;;) {
      if (signal.aborted || this.stopped) return false
      const at = this.now()
      const got = await this.store.claimWriter(
        this.owner,
        at + WRITER_LEASE_MS,
        (row) => row === undefined || row.until <= at || (via === 'lock' && row.via === 'lock'),
        via,
      )
      if (got) return true
      if (!wait) return false
      await this.sleep(signal)
    }
  }

  // With Web Locks: the lock is taken first, and held for as long as the promise the callback
  // returns is pending; the row is then taken on top of it. A lock that is granted after this tab
  // stopped (or after the wait was cancelled) is let go of at once, with the row if it got it.
  private acquireLock(wait: boolean, signal: AbortSignal): Promise<void> {
    const locks = this.locks as LockManager
    return new Promise<void>((resolve) => {
      // `ifAvailable` and `signal` can't be asked together: a start doesn't wait, a take-over does.
      const options: LockOptions = wait ? { signal } : { ifAvailable: true }
      locks
        .request(this.name, options, async (lock) => {
          if (!lock) {
            resolve()
            return
          }
          let got = false
          try {
            got = await this.takeRow(wait, signal, 'lock')
          } catch {
            // The database is closed.
          }
          if (!got) {
            // Not ours to hold: returning lets the lock go.
            resolve()
            return
          }
          if (signal.aborted || this.stopped) {
            await this.store.releaseWriter(this.owner).catch(() => {})
            resolve()
            return
          }
          await new Promise<void>((release) => {
            this.unlock = release
            this.renewing('lock')
            resolve()
          })
        })
        .catch(() => resolve())
    })
  }

  // Without them: the row's lease, taken when it is free or has run out, and renewed while held.
  private async acquireLease(wait: boolean, signal: AbortSignal): Promise<void> {
    if (!(await this.takeRow(wait, signal, 'lease'))) return
    if (signal.aborted || this.stopped) {
      await this.store.releaseWriter(this.owner).catch(() => {})
      return
    }
    this.renewing('lease')
  }

  // The row is this tab's: it is the writer, and keeps the lease renewed for as long as it is.
  private renewing(via: 'lock' | 'lease') {
    this.set(!this.stopped)
    this.stopBeat?.()
    this.stopBeat = (this.deps.heartbeat ?? realHeartbeat)(() => {
      const at = this.now()
      this.store
        .claimWriter(this.owner, at + WRITER_LEASE_MS, () => false, via)
        .then(
          (still) => {
            if (!still) this.lost()
          },
          () => this.lost(),
        )
    }, WRITER_LEASE_MS / 3)
  }

  private sleep(signal: AbortSignal): Promise<void> {
    return new Promise<void>((resolve) => {
      const stop = () => {
        this.clearPoll()
        resolve()
      }
      signal.addEventListener('abort', stop, { once: true })
      this.poll = (this.deps.setTimer ?? setTimeout)(() => {
        this.poll = undefined
        signal.removeEventListener('abort', stop)
        resolve()
      }, POLL_MS)
    })
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

  // The right was taken from this tab (or the database can't say): it is a reader, and a Web Lock
  // it holds is let go, so that it isn't held by a tab that doesn't write.
  private lost() {
    this.stopBeat?.()
    this.stopBeat = undefined
    this.set(false)
    const unlock = this.unlock
    this.unlock = undefined
    unlock?.()
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

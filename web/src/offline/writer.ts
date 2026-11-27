import type { OfflineStore } from './store'

export const WRITER_LEASE_MS = 15_000
const POLL_MS = 400

export type WriterDeps = {
  now?: () => number
  heartbeat?: (fn: () => void, ms: number) => () => void
  setTimer?: (fn: () => void, ms: number) => unknown
  clearTimer?: (handle: unknown) => void
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
  private epoch = 0
  private listeners = new Set<() => void>()
  private stopped = false
  private unlock: (() => void) | undefined
  private stopBeat: (() => void) | undefined
  private waiting: AbortController | undefined
  private acquiring: AbortController | undefined
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

  holds(): boolean {
    return this.held && !this.stopped
  }

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

  async takeOver(): Promise<void> {
    if (this.holds()) return
    this.waiting?.abort()
    const wait = new AbortController()
    this.waiting = wait
    try {
      this.chan?.postMessage({ want: this.owner })
    } catch {}
    try {
      await this.acquire(true, wait)
    } finally {
      if (this.waiting === wait) this.waiting = undefined
    }
  }

  stop() {
    this.stopped = true
    this.waiting?.abort()
    this.acquiring?.abort()
    this.clearPoll()
    void this.release()
    try {
      this.chan?.close()
    } catch {}
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
    } finally {
      if (this.acquiring === ctl) this.acquiring = undefined
    }
  }

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

  private acquireLock(wait: boolean, signal: AbortSignal): Promise<void> {
    const locks = this.locks as LockManager
    return new Promise<void>((resolve) => {
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
          } catch {}
          if (!got) {
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

  private async acquireLease(wait: boolean, signal: AbortSignal): Promise<void> {
    if (!(await this.takeRow(wait, signal, 'lease'))) return
    if (signal.aborted || this.stopped) {
      await this.store.releaseWriter(this.owner).catch(() => {})
      return
    }
    this.renewing('lease')
  }

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

  private async yieldWriter() {
    if (!this.held) return
    this.set(false)
    await this.release()
  }

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
    } catch {}
    unlock?.()
  }
}

const realHeartbeat = (fn: () => void, ms: number) => {
  const handle = setInterval(fn, ms)
  return () => clearInterval(handle)
}

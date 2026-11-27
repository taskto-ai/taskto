import { DatabaseSync } from 'node:sqlite'
import { isMainThread, parentPort, Worker, workerData } from 'node:worker_threads'

export class WalBusyError extends Error {}

export function isContention(error: unknown): boolean {
  const code = (error as { errcode?: unknown } | null)?.errcode
  return typeof code === 'number' && ((code & 0xff) === 5 || (code & 0xff) === 6)
}

export function truncateWal(db: DatabaseSync): void {
  let busy: number
  try {
    const row = db.prepare('PRAGMA wal_checkpoint(TRUNCATE)').get() as { busy: number | bigint } | undefined
    busy = Number(row?.busy ?? 1)
  } catch (error) {
    if (!isContention(error)) throw error
    throw new WalBusyError(`the write-ahead log couldn't be checkpointed: ${(error as Error).message}`)
  }
  if (busy !== 0) throw new WalBusyError('a reader holds the write-ahead log, so it could not be truncated')
}

function sleep(ms: number): void {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
}

const PAUSE_MS = 30

const DRAINED_FRAMES = 64

type CheckpointRow = { busy: number | bigint; log: number | bigint; checkpointed: number | bigint }

export function framesLeft(row: CheckpointRow): number {
  if (Number(row.busy) !== 0) return Number.POSITIVE_INFINITY
  const log = Number(row.log)
  if (log < 0) return 0
  return log - Number(row.checkpointed)
}

function passive(db: DatabaseSync): number {
  const row = db.prepare('PRAGMA wal_checkpoint(PASSIVE)').get() as CheckpointRow | undefined
  if (!row) throw new Error('the checkpoint returned no row')
  return framesLeft(row)
}

function backlog(db: DatabaseSync): number {
  try {
    return passive(db)
  } catch (error) {
    if (isContention(error)) return Number.POSITIVE_INFINITY
    throw error
  }
}

const QUICK_MS = 20

export type TruncateHooks = {
  round?: (backlog: number, ms: number) => void
  truncating?: (backlog: number) => void
  truncated?: (ms: number) => void
}

export function truncateWalWithin(db: DatabaseSync, timeoutMs: number, hooks?: TruncateHooks): void {
  db.exec('PRAGMA busy_timeout = 0')
  const deadline = performance.now() + timeoutMs
  let last: unknown
  for (;;) {
    const began = performance.now()
    const left = backlog(db)
    const took = performance.now() - began
    hooks?.round?.(left, took)
    const quickAgain = () => {
      const again = performance.now()
      return backlog(db) <= DRAINED_FRAMES && performance.now() - again <= QUICK_MS
    }
    if (left <= DRAINED_FRAMES && took <= QUICK_MS && quickAgain()) {
      hooks?.truncating?.(left)
      try {
        const t0 = performance.now()
        truncateWal(db)
        hooks?.truncated?.(performance.now() - t0)
        return
      } catch (error) {
        if (!(error instanceof WalBusyError)) throw error
        last = error
      }
    }
    if (performance.now() + PAUSE_MS > deadline) {
      throw last instanceof WalBusyError
        ? last
        : new WalBusyError('a reader holds the write-ahead log, so it could not be copied and truncated')
    }
    sleep(PAUSE_MS)
  }
}

export function truncateWalAt(path: string, timeoutMs: number): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    truncateWalWithin(db, timeoutMs)
  } finally {
    db.close()
  }
}

export const LIFECYCLE_KIND = 'taskto.lifecycle'

export function storedKind(kind: string): string {
  return JSON.stringify(kind)
}

const LIFECYCLE = `'${storedKind(LIFECYCLE_KIND).replaceAll("'", "''")}'`

export const CHUNK = 50

const SWEEP = CHUNK

export const SLICE_STEPS = 64

const SLICE_MS = 20

export const SLICE_PAGES = 512

export type SliceTiming = {
  lock: number
  work: number
  commit: number
  pages: number
  step: number
  steps: number
  records: number
  changes: number
}

export type FinalWork = { changes: number; pages: number }

export type PurgeHooks = {
  afterSlice?: () => void
  slice?: (ms: number, timing: SliceTiming) => void
  final?: (ms: number, work: FinalWork) => void
  truncate?: (ms: number) => void
}

const NO_ROOM = -1

export function rowPages(bytes: number, pageSize: number): number {
  const cell = bytes + 24
  const usable = pageSize
  const maxLocal = usable - 35
  const minLocal = Math.floor(((usable - 12) * 32) / 255) - 23
  let pages: number
  if (cell <= maxLocal) {
    pages = 1 / Math.max(1, Math.floor((usable - 8) / (cell + 4)))
  } else {
    let local = minLocal + ((cell - minLocal) % (usable - 4))
    if (local > maxLocal) local = minLocal
    pages = Math.ceil((cell - local) / (usable - 4)) + (local + 4) / (usable - 8)
  }
  return pages * ROW_MARGIN
}

const ROW_MARGIN = 1.25

function rowBytes(db: DatabaseSync, table: string): string {
  let known = sizeExpressions.get(db)
  if (!known) {
    known = new Map()
    sizeExpressions.set(db, known)
  }
  let expression = known.get(table)
  if (expression === undefined) {
    const columns = db.prepare(`PRAGMA table_info(${table})`).all() as { name: string }[]
    expression = columns.map((c) => `coalesce(octet_length("${c.name.replaceAll('"', '""')}"), 0)`).join(' + ') || '0'
    known.set(table, expression)
  }
  return expression
}

const sizeExpressions = new WeakMap<DatabaseSync, Map<string, string>>()

class PageBudget {
  private planned = 0
  private measured = 0
  private deleted = false
  private readonly freePages: () => number
  readonly pageSize: number
  constructor(freePages: () => number, pageSize: number) {
    this.freePages = freePages
    this.pageSize = pageSize
  }

  begin(): void {
    this.planned = 0
    this.measured = 0
    this.deleted = false
  }

  freed(): number {
    return this.measured
  }

  take(sizes: number[], remove: (count: number) => void): number {
    if (sizes.length === 0) return 0
    let count = 0
    let planned = this.planned
    for (const bytes of sizes) {
      const pages = rowPages(bytes, this.pageSize)
      if (planned + pages > SLICE_PAGES && (count > 0 || this.deleted)) break
      planned += pages
      count++
    }
    if (count === 0) return NO_ROOM
    const before = this.freePages()
    remove(count)
    this.measured += Math.max(0, this.freePages() - before)
    this.planned = planned
    this.deleted = true
    return count
  }
}

export function purgeRecords(path: string, root: number, timeoutMs: number, hooks?: PurgeHooks): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    db.exec('PRAGMA secure_delete = ON')
    db.exec('PRAGMA synchronous = NORMAL')
    db.exec('PRAGMA wal_autocheckpoint = 0')
    db.exec(MARKS_TABLE)
    db.exec(MARKS_INDEX)
    db.prepare(`INSERT OR IGNORE INTO taskto_purge_marks (root, kind, id) VALUES (?, 'conversation', ?)`).run(
      root,
      root,
    )
    const freelist = db.prepare('PRAGMA freelist_count')
    const freePages = () => Number((freelist.get() as { freelist_count: number | bigint }).freelist_count)
    const totalChanges = db.prepare('SELECT total_changes() AS n')
    const changes = () => Number((totalChanges.get() as { n: number | bigint }).n)
    const pageSize = Number((db.prepare('PRAGMA page_size').get() as { page_size: number | bigint }).page_size)
    const budget = new PageBudget(freePages, pageSize)
    const step = sweepStep(db, root, budget)
    for (let more = true; more; ) {
      const began = performance.now()
      db.exec('BEGIN IMMEDIATE')
      const locked = performance.now()
      let worked = locked
      let freed = 0
      let longestStep = 0
      let steps = 0
      let records = 0
      let mostChanged = 0
      try {
        budget.begin()
        while (steps < SLICE_STEPS && performance.now() - locked < SLICE_MS) {
          const t0 = performance.now()
          const c0 = changes()
          const n = step()
          longestStep = Math.max(longestStep, performance.now() - t0)
          if (n === NO_ROOM) break
          steps++
          records = Math.max(records, n)
          mostChanged = Math.max(mostChanged, changes() - c0)
          if (n === 0) {
            more = false
            break
          }
        }
        freed = budget.freed()
        worked = performance.now()
        db.exec('COMMIT')
      } catch (error) {
        db.exec('ROLLBACK')
        throw error
      }
      const ended = performance.now()
      hooks?.slice?.(ended - began, {
        lock: locked - began,
        work: worked - locked,
        commit: ended - worked,
        pages: freed,
        step: longestStep,
        steps,
        records,
        changes: mostChanged,
      })
      hooks?.afterSlice?.()
      backlog(db)
      sleep(PAUSE_MS)
    }
    const began = performance.now()
    db.exec('BEGIN IMMEDIATE')
    let finalWork: FinalWork
    try {
      const c0 = changes()
      const f0 = freePages()
      const tree = discover(db, root, marked(db, root))
      for (const step of contentSteps(db, tree)) while (step() > 0);
      const ids = list(tree.ids)
      removeDocuments(db, select(db, `SELECT id FROM documents WHERE ${ownedBy(tree)}`))
      removeRows(db, 'tasks', select(db, `SELECT id FROM tasks WHERE conversation_id IN (${ids})`))
      removeRows(db, 'conversations', select(db, `SELECT id FROM conversations WHERE id IN (${ids})`))
      db.prepare('DELETE FROM taskto_purge_marks WHERE root = ?').run(root)
      finalWork = { changes: changes() - c0, pages: freePages() - f0 }
      db.exec('COMMIT')
    } catch (error) {
      db.exec('ROLLBACK')
      throw error
    }
    hooks?.final?.(performance.now() - began, finalWork)
    truncateWalWithin(db, timeoutMs, hooks?.truncate ? { truncated: hooks.truncate } : undefined)
  } finally {
    db.close()
  }
}

const MARKS_TABLE = `CREATE TABLE IF NOT EXISTS taskto_purge_marks (
  root INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('conversation', 'task')),
  id   INTEGER NOT NULL,
  done INTEGER NOT NULL DEFAULT 0 CHECK (done IN (0, 1)),
  PRIMARY KEY (root, kind, id)
) STRICT`

const MARKS_INDEX =
  'CREATE INDEX IF NOT EXISTS taskto_purge_marks_by_state ON taskto_purge_marks (root, done, kind, id)'

function marked(db: DatabaseSync, root: number): Tree {
  const tree: Tree = { ids: new Set(), taskIds: new Set() }
  for (const row of db.prepare('SELECT kind, id FROM taskto_purge_marks WHERE root = ?').all(root) as {
    kind: string
    id: number | bigint
  }[]) {
    ;(row.kind === 'task' ? tree.taskIds : tree.ids).add(Number(row.id))
  }
  return tree
}

type Tree = { ids: Set<number>; taskIds: Set<number> }

function list(set: Set<number> | number[]): string {
  return [...set].join(',') || 'NULL'
}

function select(db: DatabaseSync, query: string): number[] {
  return (db.prepare(query).all() as { id: number | bigint }[]).map((r) => Number(r.id))
}

function removeRows(db: DatabaseSync, table: string, ids: number[], records = true): void {
  if (ids.length === 0) return
  db.exec(`DELETE FROM ${table} WHERE rowid IN (${ids.join(',')})`)
  if (records) db.exec(`DELETE FROM record_ids WHERE id IN (${ids.join(',')})`)
}

function removeDocuments(db: DatabaseSync, ids: number[]): void {
  if (ids.length === 0) return
  db.exec(`DELETE FROM document_revisions WHERE document_id IN (${ids.join(',')})`)
  removeRows(db, 'documents', ids)
}

function ownedBy(tree: Tree): string {
  return `((scope_kind = 'conversation' AND owner_id IN (${list(tree.ids)}))
    OR (scope_kind = 'task' AND owner_id IN (${list(tree.taskIds)})))`
}

type Step = () => number

function each(
  db: DatabaseSync,
  table: string,
  key: 'id' | 'rowid',
  where: string,
  remove: (chunk: number[]) => void,
  budget?: PageBudget,
): Step {
  if (!budget) {
    return () => {
      const chunk = select(db, `SELECT ${key} AS id FROM ${table} WHERE ${where} LIMIT ${CHUNK}`)
      remove(chunk)
      return chunk.length
    }
  }
  const sized = `SELECT ${key} AS id, ${rowBytes(db, table)} AS bytes FROM ${table} WHERE ${where} LIMIT ${CHUNK}`
  return () => {
    const rows = db.prepare(sized).all() as { id: number | bigint; bytes: number | bigint }[]
    return budget.take(
      rows.map((r) => Number(r.bytes)),
      (count) => remove(rows.slice(0, count).map((r) => Number(r.id))),
    )
  }
}

function contentSteps(db: DatabaseSync, tree: Tree): Step[] {
  const ids = list(tree.ids)
  const docs = `${ownedBy(tree)} AND kind <> ${LIFECYCLE}`
  return [
    each(db, 'document_revisions', 'rowid', `document_id IN (SELECT id FROM documents WHERE ${docs})`, (c) =>
      removeRows(db, 'document_revisions', c, false),
    ),
    each(db, 'documents', 'id', docs, (c) => removeDocuments(db, c)),
    each(db, 'submissions', 'id', `conversation_id IN (${ids})`, (c) => removeRows(db, 'submissions', c)),
    each(db, 'entries', 'id', `conversation_id IN (${ids})`, (c) => removeRows(db, 'entries', c)),
  ]
}

type Mark = { r: number | bigint; kind: string; id: number | bigint }

function sweepStep(db: DatabaseSync, root: number, budget: PageBudget): Step {
  const batchOf = db.prepare(
    `SELECT rowid AS r, kind, id FROM taskto_purge_marks WHERE root = ? AND done = ?
      ORDER BY kind DESC, id DESC LIMIT ${SWEEP}`,
  )
  const isRoot = (m: Mark) => m.kind === 'conversation' && Number(m.id) === root
  const lifecycle = `NOT (scope_kind = 'conversation' AND owner_id = ${root} AND kind = ${LIFECYCLE})`
  const work = (batch: Mark[]): number => {
    const c = list(batch.filter((m) => m.kind === 'conversation').map((m) => Number(m.id)))
    const t = list(batch.filter((m) => m.kind === 'task').map((m) => Number(m.id)))
    const started = db
      .prepare(`INSERT OR IGNORE INTO taskto_purge_marks (root, kind, id)
        SELECT ?, 'task', x.id FROM tasks x WHERE x.conversation_id IN (${c})
          AND NOT EXISTS (SELECT 1 FROM taskto_purge_marks m WHERE m.root = ? AND m.kind = 'task' AND m.id = x.id)
        LIMIT ${SWEEP}`)
      .run(root, root)
    if (Number(started.changes) > 0) return Number(started.changes)
    const owned = db
      .prepare(`INSERT OR IGNORE INTO taskto_purge_marks (root, kind, id)
        SELECT ?, 'conversation', x.id FROM conversations x WHERE x.owner_task_id IN (${t})
          AND NOT EXISTS (SELECT 1 FROM taskto_purge_marks m WHERE m.root = ? AND m.kind = 'conversation' AND m.id = x.id)
        LIMIT ${SWEEP}`)
      .run(root, root)
    if (Number(owned.changes) > 0) return Number(owned.changes)
    const docs = `((scope_kind = 'conversation' AND owner_id IN (${c}))
      OR (scope_kind = 'task' AND owner_id IN (${t}))) AND ${lifecycle}`
    for (const step of [
      each(
        db,
        'document_revisions',
        'rowid',
        `document_id IN (SELECT id FROM documents WHERE ${docs})`,
        (x) => removeRows(db, 'document_revisions', x, false),
        budget,
      ),
      each(db, 'documents', 'id', docs, (x) => removeDocuments(db, x), budget),
      each(db, 'submissions', 'id', `conversation_id IN (${c})`, (x) => removeRows(db, 'submissions', x), budget),
      each(db, 'entries', 'id', `conversation_id IN (${c})`, (x) => removeRows(db, 'entries', x), budget),
    ]) {
      const n = step()
      if (n !== 0) return n
    }
    return 0
  }
  return () => {
    const todo = batchOf.all(root, 0) as Mark[]
    if (todo.length > 0) {
      const n = work(todo)
      if (n !== 0) return n
      const others = todo.filter((m) => !isRoot(m))
      if (others.length > 0) {
        const bytes = new Map<string, number>()
        const sizes = (table: string, kind: string) => {
          const ids = list(others.filter((m) => m.kind === kind).map((m) => Number(m.id)))
          for (const row of db
            .prepare(`SELECT id, ${rowBytes(db, table)} AS bytes FROM ${table} WHERE id IN (${ids})`)
            .all() as {
            id: number | bigint
            bytes: number | bigint
          }[])
            bytes.set(`${kind}:${Number(row.id)}`, Number(row.bytes))
        }
        sizes('tasks', 'task')
        sizes('conversations', 'conversation')
        return budget.take(
          others.map((m) => bytes.get(`${m.kind}:${Number(m.id)}`) ?? 0),
          (count) => {
            const some = others.slice(0, count)
            const t = list(some.filter((m) => m.kind === 'task').map((m) => Number(m.id)))
            const c = list(some.filter((m) => m.kind === 'conversation').map((m) => Number(m.id)))
            removeRows(db, 'tasks', select(db, `SELECT id FROM tasks WHERE id IN (${t})`))
            removeRows(db, 'conversations', select(db, `SELECT id FROM conversations WHERE id IN (${c})`))
            db.exec(`UPDATE taskto_purge_marks SET done = 1 WHERE rowid IN (${some.map((m) => Number(m.r)).join(',')})`)
          },
        )
      }
    }
    const done = batchOf.all(root, 1) as Mark[]
    if (done.length === 0) return 0
    const n = work(done)
    if (n !== 0) return n
    db.exec(`DELETE FROM taskto_purge_marks WHERE rowid IN (${done.map((m) => Number(m.r)).join(',')})`)
    return done.length
  }
}

function discover(db: DatabaseSync, root: number, seen?: Tree): Tree {
  const ids = new Set<number>([root, ...(seen?.ids ?? [])])
  const taskIds = new Set<number>(seen?.taskIds ?? [])
  for (let grew = true; grew; ) {
    grew = false
    const tasks = db.prepare(`SELECT id FROM tasks WHERE conversation_id IN (${list(ids)})`).all() as {
      id: number
    }[]
    for (const t of tasks) taskIds.add(Number(t.id))
    const subs = db.prepare(`SELECT id FROM conversations WHERE owner_task_id IN (${list(taskIds)})`).all() as {
      id: number
    }[]
    for (const c of subs) {
      if (!ids.has(Number(c.id))) {
        ids.add(Number(c.id))
        grew = true
      }
    }
  }
  return { ids, taskIds }
}

export type PurgeJob = {
  path: string
  timeoutMs: number
  root?: number
  report?: boolean
}

export type PurgeReport = {
  slices: number
  longest: SliceTiming & { ms: number }
  lock: number
  work: number
  commit: number
  step: number
  pages: number
  records: number
  changes: number
  final?: number
  truncate?: number
}

type Outcome = { ok: true; report?: PurgeReport } | { ok: false; busy: boolean; message: string }

const MARK = 'taskto.purge'

function run(job: PurgeJob): Outcome {
  try {
    if (job.root === undefined) {
      truncateWalAt(job.path, job.timeoutMs)
      return { ok: true }
    }
    let report: PurgeReport | undefined
    const slice = (ms: number, t: SliceTiming) => {
      report ??= {
        slices: 0,
        longest: { ms, ...t },
        lock: 0,
        work: 0,
        commit: 0,
        step: 0,
        pages: 0,
        records: 0,
        changes: 0,
      }
      report.slices++
      if (ms > report.longest.ms) report.longest = { ms, ...t }
      report.lock = Math.max(report.lock, t.lock)
      report.work = Math.max(report.work, t.work)
      report.commit = Math.max(report.commit, t.commit)
      report.step = Math.max(report.step, t.step)
      report.pages = Math.max(report.pages, t.pages)
      report.records = Math.max(report.records, t.records)
      report.changes = Math.max(report.changes, t.changes)
    }
    let final = 0
    let truncate = 0
    purgeRecords(
      job.path,
      job.root,
      job.timeoutMs,
      job.report ? { slice, final: (ms) => (final = ms), truncate: (ms) => (truncate = ms) } : undefined,
    )
    if (report) Object.assign(report, { final, truncate })
    return { ok: true, report }
  } catch (error) {
    return { ok: false, busy: error instanceof WalBusyError, message: (error as Error).message ?? String(error) }
  }
}

let workers = 0

export function purgeWorkersStarted(): number {
  return workers
}

export function runPurge(job: PurgeJob): Promise<PurgeReport | undefined> {
  workers++
  running++
  mostAtOnce = Math.max(mostAtOnce, running)
  return new Promise((resolve, reject) => {
    let settled = false
    const settle = (end: () => void) => {
      if (settled) return
      settled = true
      running--
      end()
    }
    const worker = new Worker(new URL(import.meta.url), { workerData: { [MARK]: job } })
    worker.once('message', (out: Outcome) => {
      settle(() =>
        out.ok ? resolve(out.report) : reject(out.busy ? new WalBusyError(out.message) : new Error(out.message)),
      )
    })
    worker.once('error', (error) => settle(() => reject(error)))
    worker.once('exit', (code) => settle(() => reject(new Error(`the purge's worker thread exited with code ${code}`))))
  })
}

let running = 0
let mostAtOnce = 0

export function purgeWorkersAtOnce(): number {
  const most = mostAtOnce
  mostAtOnce = running
  return most
}

const asked = new Map<string, Promise<void>>()
let queue: Promise<unknown> = Promise.resolve()

const purgeKey = (job: { path: string; root?: number }) => `${job.path}\u0000${job.root ?? 'log'}`

export function purgeInFlight(path: string, root: number): Promise<void> | undefined {
  return asked.get(purgeKey({ path, root }))
}

export function purgeOnce(job: PurgeJob): Promise<void> {
  const key = purgeKey(job)
  const running = asked.get(key)
  if (running) return running
  const result = queue.then(() => runPurge(job)).then(() => undefined)
  queue = result.catch(() => undefined)
  asked.set(key, result)
  const forget = () => {
    if (asked.get(key) === result) asked.delete(key)
  }
  result.then(forget, forget)
  return result
}

export const HARNESS_DB_OPTIONS = { walAutoCheckpointPages: 0 } as const

const CHECKPOINT_EVERY_MS = 1000

const CHECKPOINT_FAILURES = 10

const CHECKPOINTER = 'taskto.checkpointer'

type CheckpointerJob = { path: string; everyMs: number; stop: Int32Array }

export type CheckpointerHooks = {
  started?: () => void
  failed?: (count: number) => void
  restarting?: () => void
}

export function startCheckpointer(
  path: string,
  everyMs = CHECKPOINT_EVERY_MS,
  hooks?: CheckpointerHooks,
): () => Promise<void> {
  const stop = new Int32Array(new SharedArrayBuffer(4))
  let worker: Worker | undefined
  let again: ReturnType<typeof setTimeout> | undefined
  const stopped = () => Atomics.load(stop, 0) !== 0
  const start = () => {
    again = undefined
    if (stopped()) return
    const w = new Worker(new URL(import.meta.url), { workerData: { [CHECKPOINTER]: { path, everyMs, stop } } })
    worker = w
    hooks?.started?.()
    w.on('message', (m: { failed?: number }) => {
      if (typeof m?.failed === 'number') hooks?.failed?.(m.failed)
    })
    w.on('error', (error) => console.error('the checkpointer failed; it starts again', error))
    w.once('exit', () => {
      if (!stopped()) {
        again = setTimeout(start, everyMs)
        again.unref()
        hooks?.restarting?.()
      }
    })
    w.unref()
  }
  start()
  return async () => {
    Atomics.store(stop, 0, 1)
    Atomics.notify(stop, 0)
    if (again !== undefined) clearTimeout(again)
    const w = worker
    if (!w || w.threadId === -1) return
    w.ref()
    await new Promise<void>((resolve) => {
      w.once('exit', () => resolve())
      if (w.threadId === -1) resolve()
    })
  }
}

function checkpoint(job: CheckpointerJob): void {
  const db = new DatabaseSync(job.path, { timeout: 0 })
  let failures = 0
  let failed = 0
  try {
    while (Atomics.load(job.stop, 0) === 0) {
      try {
        if (passive(db) !== Number.POSITIVE_INFINITY) failures = 0
      } catch (error) {
        failures++
        failed++
        console.error(`checkpoint failed (${failed} so far, ${failures} in a row): ${(error as Error).message}`)
        parentPort?.postMessage({ failed })
        if (failures >= CHECKPOINT_FAILURES) throw new Error(`${failures} checkpoints in a row failed`)
      }
      Atomics.wait(job.stop, 0, 0, job.everyMs)
    }
  } finally {
    db.close()
  }
}

const data = isMainThread ? null : (workerData as Record<string, unknown> | null)
const mine = data?.[MARK]
if (mine && parentPort) parentPort.postMessage(run(mine as PurgeJob))
const checkpointing = data?.[CHECKPOINTER]
if (checkpointing) checkpoint(checkpointing as CheckpointerJob)

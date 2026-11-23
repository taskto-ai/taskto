// IndexedDB without a library: just enough of it, as promises.
//
// A write is done when its transaction has completed (`oncomplete`), never when its request
// succeeded: a request that succeeds in a transaction that is later aborted (the quota, a closed
// tab) was not kept. Whatever the offline code tells the person was saved is told after
// `writeTx` resolves, and a rejection (QuotaExceededError included) is the failure to show.

export class StorageUnavailable extends Error {
  constructor(message = 'IndexedDB is not available') {
    super(message)
    this.name = 'StorageUnavailable'
  }
}

// The database on disk is of a newer schema than this code knows: another tab or an installed
// version newer than this page wrote it. It is left alone, and the page asks to be updated.
export class SchemaTooNew extends Error {
  constructor() {
    super('the offline database is of a newer version')
    this.name = 'SchemaTooNew'
  }
}

// Whether e is the browser saying the storage is full.
export function isQuotaError(e: unknown): boolean {
  if (!(e instanceof Error) && !(typeof DOMException !== 'undefined' && e instanceof DOMException)) return false
  const name = (e as { name?: string }).name
  const code = (e as { code?: number }).code
  return name === 'QuotaExceededError' || code === 22 || /quota/i.test(String((e as Error).message))
}

export type Migration = (db: IDBDatabase, tx: IDBTransaction) => void

// Opens name at version, running the migrations (keyed by the version they upgrade *to*) of every
// version between the one on disk and version, in order, in the upgrade's own transaction.
export function openDatabase(
  name: string,
  version: number,
  migrations: Record<number, Migration>,
): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    let request: IDBOpenDBRequest
    try {
      if (typeof indexedDB === 'undefined') throw new StorageUnavailable()
      request = indexedDB.open(name, version)
    } catch (e) {
      reject(e instanceof StorageUnavailable ? e : new StorageUnavailable(String(e)))
      return
    }
    request.onupgradeneeded = (event) => {
      const db = request.result
      const tx = request.transaction as IDBTransaction
      for (let v = event.oldVersion + 1; v <= (event.newVersion ?? version); v++) migrations[v]?.(db, tx)
    }
    request.onsuccess = () => {
      const db = request.result
      // Another tab upgrading it, or deleting it (a sign-out there), needs this connection closed.
      db.onversionchange = () => db.close()
      resolve(db)
    }
    request.onerror = () => {
      const error = request.error
      reject(error?.name === 'VersionError' ? new SchemaTooNew() : (error ?? new StorageUnavailable()))
    }
    request.onblocked = () => reject(new StorageUnavailable('the offline database is in use by another tab'))
  })
}

export function deleteDatabase(name: string): Promise<void> {
  return new Promise((resolve, reject) => {
    if (typeof indexedDB === 'undefined') return resolve()
    const request = indexedDB.deleteDatabase(name)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error)
    // Blocked: other connections are closing (onversionchange); the deletion goes on once they have.
    request.onblocked = () => {}
  })
}

const done = <T>(request: IDBRequest<T>): Promise<T> =>
  new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })

export type Tx = { store<T = unknown>(name: string): Store<T> }

export type Store<T> = {
  get(key: IDBValidKey): Promise<T | undefined>
  getAll(query?: IDBValidKey | IDBKeyRange): Promise<T[]>
  getAllByIndex(index: string, query: IDBValidKey | IDBKeyRange): Promise<T[]>
  put(value: T): Promise<void>
  delete(key: IDBValidKey): Promise<void>
  count(): Promise<number>
}

function wrap<T>(os: IDBObjectStore): Store<T> {
  return {
    get: (key) => done(os.get(key)) as Promise<T | undefined>,
    getAll: (query) => done(os.getAll(query)) as Promise<T[]>,
    getAllByIndex: (index, query) => done(os.index(index).getAll(query)) as Promise<T[]>,
    put: (value) => done(os.put(value)).then(() => undefined),
    delete: (key) => done(os.delete(key)),
    count: () => done(os.count()),
  }
}

// Runs fn in one transaction over stores and resolves with its result once the transaction has
// completed. A write that is refused (quota) or a transaction that is aborted rejects, and none of
// its writes is kept. `strict` durability asks the browser to flush to disk before completing,
// where it can.
export function runTx<R>(
  db: IDBDatabase,
  stores: string[],
  mode: IDBTransactionMode,
  fn: (tx: Tx) => Promise<R>,
): Promise<R> {
  return new Promise<R>((resolve, reject) => {
    let tx: IDBTransaction
    try {
      tx = db.transaction(stores, mode, mode === 'readwrite' ? { durability: 'strict' } : undefined)
    } catch (e) {
      reject(e)
      return
    }
    let result: R
    let failed = false
    const fail = (e: unknown) => {
      if (failed) return
      failed = true
      try {
        tx.abort()
      } catch {
        // Already finished or aborted.
      }
      reject(e)
    }
    tx.oncomplete = () => {
      if (!failed) resolve(result)
    }
    tx.onabort = () => fail(tx.error ?? new DOMException('the transaction was aborted', 'AbortError'))
    tx.onerror = () => fail(tx.error)
    fn({ store: (name) => wrap(tx.objectStore(name)) }).then((r) => {
      result = r
    }, fail)
  })
}

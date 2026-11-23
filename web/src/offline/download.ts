// "Download for offline use": the app's live release and the records the person may read, kept on
// this device. What is downloaded is the person's view at that moment and says so (when, which
// release, how many records of how many): every count the app shows offline is over that copy and
// what is queued on this device, never the server's totals.

import { api, forAccount, type OfflineApp } from '../api'
import { unsupportedBy } from '../apps/catalog'
import { assertSignedIn } from './account'
import { type Download, type OfflineStore, type Scope, scopeKey } from './store'

export type StorageStatus = {
  // Bytes in use and the most this site may use, as the browser estimates (it may not say).
  usage?: number
  quota?: number
  // Whether the browser promised to keep the site's data: without it, it may clear it under
  // storage pressure.
  persisted?: boolean
}

export async function storageStatus(): Promise<StorageStatus> {
  const out: StorageStatus = {}
  try {
    const est = await navigator.storage?.estimate?.()
    if (est) Object.assign(out, { usage: est.usage, quota: est.quota })
  } catch {
    // The browser doesn't say.
  }
  try {
    out.persisted = await navigator.storage?.persisted?.()
  } catch {
    // Nor that.
  }
  return out
}

// Asks the browser to keep this site's data. It improves the odds, and promises nothing: the
// answer is shown, and the app works the same either way.
export async function requestPersistence(): Promise<boolean> {
  try {
    return (await navigator.storage?.persist?.()) ?? false
  } catch {
    return false
  }
}

const appBase = (s: Scope) => `/api/workspaces/${s.workspace}/spaces/${s.space}/apps/${s.app}`

// Downloads the app and keeps it. Rejects with the server's error (an admin turned downloads off:
// `offline_downloads_disabled`; the session is another account's than the database's:
// `session_account_changed`) or a NotSaved from the device (the storage is full). The request
// names the account whose database the answer is for, and the answer is written only while the
// browser is still signed in as them (`AccountChanged` otherwise).
export async function downloadApp(store: OfflineStore, scope: Scope): Promise<Download> {
  const r = await api<OfflineApp>(`${appBase(scope)}/offline`, { headers: forAccount(store.user) })
  // A release this client can't draw is not kept as if it could be used.
  const missing = unsupportedBy(r.definition)
  if (missing.length > 0) throw new UnsupportedRelease(missing)
  const download: Download = {
    ...scope,
    key: scopeKey(scope),
    user: store.user,
    name: r.name,
    release: r.release,
    definition: r.definition,
    messages: r.messages,
    scopeOf: r.scope,
    records: r.records,
    total: r.total,
    truncated: r.truncated,
    canInsert: r.can_insert,
    capturedAt: r.captured_at,
    operations: r.operations ?? {},
    modifiable: r.modifiable,
    bytes: JSON.stringify(r.messages).length + JSON.stringify(r.definition).length,
  }
  // The copy and the records of this device it already holds, in one step: a record the server
  // has (it is among this download's) is the server's from now on, and the device's own entry for
  // it, which would show it twice, goes.
  assertSignedIn(store.user)
  await store.putDownload(download)
  await store.dropIncluded(scope, download.operations, presentIn(download))
  // After the copy is kept: improves its chance to stay.
  void requestPersistence()
  return download
}

// The ids of the records a download holds, when it holds them all (a truncated one doesn't say
// that a record it lacks is gone).
function presentIn(d: Download): ReadonlySet<number> | null {
  if (d.truncated) return null
  const ids = new Set<number>()
  for (const m of d.messages as { updateDataModel?: { path?: string; value?: { items?: unknown } } }[]) {
    const items = m.updateDataModel?.path === '/' ? m.updateDataModel.value?.items : undefined
    for (const i of Array.isArray(items) ? items : []) if (typeof i?.id === 'number') ids.add(i.id)
  }
  return ids
}

export class UnsupportedRelease extends Error {
  constructor(readonly missing: string[]) {
    super('this client lacks what the release needs')
    this.name = 'UnsupportedRelease'
  }
}

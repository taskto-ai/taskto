// "Download for offline use": the app's live release and the records the person may read, kept on
// this device. What is downloaded is the person's view at that moment and says so (when, which
// release, how many records of how many): every count the app shows offline is over that copy and
// what is queued on this device, never the server's totals.

import { api, type OfflineApp } from '../api'
import { unsupportedBy } from '../apps/catalog'
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
// `offline_downloads_disabled`) or a NotSaved from the device (the storage is full).
export async function downloadApp(store: OfflineStore, scope: Scope): Promise<Download> {
  const r = await api<OfflineApp>(`${appBase(scope)}/offline`)
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
    bytes: JSON.stringify(r.messages).length + JSON.stringify(r.definition).length,
  }
  // The copy and the records of this device it already holds, in one step: a record the server
  // has (it is among this download's) is the server's from now on, and the device's own entry for
  // it, which would show it twice, goes.
  await store.putDownload(download)
  await store.dropIncluded(scope, download.operations)
  // After the copy is kept: improves its chance to stay.
  void requestPersistence()
  return download
}

export class UnsupportedRelease extends Error {
  constructor(readonly missing: string[]) {
    super('this client lacks what the release needs')
    this.name = 'UnsupportedRelease'
  }
}

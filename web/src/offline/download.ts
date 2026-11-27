import { api, forAccount, type OfflineApp } from '../api'
import { unsupportedBy } from '../apps/capabilities'
import { assertSignedIn } from './account'
import { type Download, type OfflineStore, type Scope, StaleDownload, scopeKey } from './store'

export type StorageStatus = {
  usage?: number
  quota?: number
  persisted?: boolean
}

export async function storageStatus(): Promise<StorageStatus> {
  const out: StorageStatus = {}
  try {
    const est = await navigator.storage?.estimate?.()
    if (est) Object.assign(out, { usage: est.usage, quota: est.quota })
  } catch {}
  try {
    out.persisted = await navigator.storage?.persisted?.()
  } catch {}
  return out
}

export async function requestPersistence(): Promise<boolean> {
  try {
    return (await navigator.storage?.persist?.()) ?? false
  } catch {
    return false
  }
}

const appBase = (s: Scope) => `/api/workspaces/${s.workspace}/spaces/${s.space}/apps/${s.app}`

export async function downloadApp(store: OfflineStore, scope: Scope): Promise<Download> {
  const ticket = await store.beginDownload()
  const r = await api<OfflineApp>(`${appBase(scope)}/offline`, { headers: forAccount(store.user) })
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
  assertSignedIn(store.user)
  try {
    await store.putDownload(download, ticket)
  } catch (e) {
    if (!(e instanceof StaleDownload)) throw e
    const held = await store.getDownload(scope)
    if (held) return held
    throw e
  }
  await store.dropIncluded(scope, download.operations, presentIn(download))
  void requestPersistence()
  return download
}

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

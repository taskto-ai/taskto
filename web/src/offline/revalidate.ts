// Asking the server whether what the device holds may still be held.

import { ApiError, type App, api, type Space } from '../api'
import { downloadApp } from './download'
import type { Download, OfflineStore, Scope } from './store'

const appBase = (s: Scope) => `/api/workspaces/${s.workspace}/spaces/${s.space}/apps/${s.app}`

type Verdict = 'keep' | 'revoked' | 'narrowed'

// Asks the server whether each downloaded copy may still be kept, and removes those it may not:
// the app can't be opened any more (the person was removed from the space or the app, it was
// moved or deleted), or an admin turned downloads off for the space. A copy that holds more than
// the person may read now (all of the app's records, where they now read only their own) is
// downloaded again, which is the server's own answer to what they read; if that can't be done it
// goes, since it can't be kept as it is. Network trouble removes nothing. What was queued for the
// app stays: it is the person's work, and it needs attention.
export async function revalidate(store: OfflineStore): Promise<Download[]> {
  const removed: Download[] = []
  for (const d of await store.listDownloads()) {
    const verdict = await check(d)
    if (verdict === 'keep') continue
    if (verdict === 'narrowed') {
      try {
        await downloadApp(store, d)
        continue
      } catch {
        // Not refreshed: what the person no longer reads is not kept.
      }
    }
    await store.removeDownload(d)
    removed.push(d)
  }
  return removed
}

// How much of an app's records a download holds, against what the person reads.
const RANK = { none: 0, own: 1, all: 2 } as const

async function check(d: Download): Promise<Verdict> {
  try {
    const space = await api<Space>(`/api/workspaces/${d.workspace}/spaces/${d.space}`)
    if (space.offline_downloads === false) return 'revoked'
    const app = await api<App>(appBase(d))
    const reads = app.access?.read_records
    if (reads !== undefined && RANK[reads] < RANK[d.scopeOf]) return 'narrowed'
    return 'keep'
  } catch (e) {
    return e instanceof ApiError && (e.status === 403 || e.status === 404) ? 'revoked' : 'keep'
  }
}

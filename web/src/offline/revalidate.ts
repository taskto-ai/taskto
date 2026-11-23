// Asking the server whether what the device holds may still be held.

import { ApiError, type App, api, type Space } from '../api'
import type { Download, OfflineStore, Scope } from './store'

const appBase = (s: Scope) => `/api/workspaces/${s.workspace}/spaces/${s.space}/apps/${s.app}`

// Asks the server whether each downloaded copy may still be kept, and removes those it may not:
// the app can't be opened any more (the person was removed from the space or the app, it was
// moved or deleted), or an admin turned downloads off for the space. Network trouble removes
// nothing. What was queued for the app stays: it is the person's work, and it needs attention.
export async function revalidate(store: OfflineStore): Promise<Download[]> {
  const removed: Download[] = []
  for (const d of await store.listDownloads()) {
    const gone = await isRevoked(d)
    if (gone) {
      await store.removeDownload(d)
      removed.push(d)
    }
  }
  return removed
}

async function isRevoked(d: Download): Promise<boolean> {
  try {
    const space = await api<Space>(`/api/workspaces/${d.workspace}/spaces/${d.space}`)
    if (space.offline_downloads === false) return true
    await api<App>(appBase(d))
    return false
  } catch (e) {
    return e instanceof ApiError && (e.status === 403 || e.status === 404)
  }
}

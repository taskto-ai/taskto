import { ApiError, type App, api, forAccount, type Space } from '../api'
import { AccountChanged, assertSignedIn } from './account'
import { downloadApp } from './download'
import type { Download, OfflineStore, Scope } from './store'

const appBase = (s: Scope) => `/api/workspaces/${s.workspace}/spaces/${s.space}/apps/${s.app}`

type Verdict = 'keep' | 'revoked' | 'narrowed'

export async function revalidate(store: OfflineStore): Promise<Download[]> {
  const removed: Download[] = []
  try {
    for (const d of await store.listDownloads()) {
      const verdict = await check(store.user, d)
      if (verdict === 'keep') continue
      if (verdict === 'narrowed') {
        try {
          await downloadApp(store, d)
          continue
        } catch (e) {
          if (isWrongAccount(e)) throw e
        }
      }
      assertSignedIn(store.user)
      await store.removeDownload(d)
      removed.push(d)
    }
  } catch (e) {
    if (!isWrongAccount(e)) throw e
  }
  return removed
}

const isWrongAccount = (e: unknown) =>
  e instanceof AccountChanged || (e instanceof ApiError && e.code === 'session_account_changed')

const RANK = { none: 0, own: 1, all: 2 } as const

async function check(user: number, d: Download): Promise<Verdict> {
  const headers = forAccount(user)
  try {
    const space = await api<Space>(`/api/workspaces/${d.workspace}/spaces/${d.space}`, { headers })
    if (space.offline_downloads === false) return 'revoked'
    const app = await api<App>(appBase(d), { headers })
    const reads = app.access?.read_records
    if (reads !== undefined && RANK[reads] < RANK[d.scopeOf]) return 'narrowed'
    return 'keep'
  } catch (e) {
    if (isWrongAccount(e)) throw e
    return e instanceof ApiError && (e.status === 403 || e.status === 404) ? 'revoked' : 'keep'
  }
}

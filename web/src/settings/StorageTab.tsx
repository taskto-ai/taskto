// Storage: what the workspace's spaces store, against the quotas the operator set
// (docs/quotas.md). Admins see every space, the members' private ones by their owner's name;
// a member sees their own private space and nothing of anyone else's, which the server
// enforces. A quota of 0 is none.

import { api, type MyStorage, type SpaceUsage, type StorageReport, usagePath } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert } from './parts'

export function StorageTab({ workspace, admin }: { workspace: number; admin: boolean }) {
  return admin ? <AdminStorage workspace={workspace} /> : <MemberStorage workspace={workspace} />
}

function AdminStorage({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const report = useLoad(() => api<StorageReport>(usagePath(workspace)), [workspace])
  const r = report.data
  // The biggest first: what an admin looks for is what fills the workspace.
  const spaces = r ? [...r.spaces].sort((a, b) => b.total - a.total || a.id - b.id) : []
  return (
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-storage">
        <h2 id="tt-storage">{t('settings.storage.title')}</h2>
        <p className="tt-muted tt-small">{t('settings.storage.intro')}</p>
        <Alert error={report.error} />
        {!r && !report.error && <p className="tt-muted">{t('app.loading')}</p>}
        {r && (
          <>
            <Meter
              label={t('settings.storage.workspace')}
              used={r.total}
              limit={r.quota.workspace}
              text={
                r.quota.workspace > 0
                  ? t('settings.storage.usedOf', { used: f.bytes(r.total), limit: f.bytes(r.quota.workspace) })
                  : t('settings.storage.usedNoLimit', { used: f.bytes(r.total) })
              }
            />
            <p className="tt-muted tt-small">
              {r.quota.user > 0
                ? t('settings.storage.userQuota', { limit: f.bytes(r.quota.user) })
                : t('settings.storage.userNoQuota')}
            </p>
          </>
        )}
      </section>
      {r && (
        <section className="tt-panel" aria-labelledby="tt-storage-spaces">
          <h2 id="tt-storage-spaces">{t('settings.storage.spaces')}</h2>
          <ul className="tt-rows">
            {spaces.map((s) => (
              <SpaceRow key={s.id} space={s} />
            ))}
          </ul>
        </section>
      )}
    </div>
  )
}

function MemberStorage({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const mine = useLoad(() => api<MyStorage>(`${usagePath(workspace)}/me`), [workspace])
  const m = mine.data
  return (
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-storage-mine">
        <h2 id="tt-storage-mine">{t('settings.storage.yours')}</h2>
        <p className="tt-muted tt-small">{t('settings.storage.yoursIntro')}</p>
        <Alert error={mine.error} />
        {!m && !mine.error && <p className="tt-muted">{t('app.loading')}</p>}
        {m && !m.space && <p className="tt-muted">{t('settings.storage.noPrivate')}</p>}
        {m?.space && (
          <ul className="tt-rows">
            <SpaceRow space={m.space} own />
          </ul>
        )}
        {m && m.quota.workspace > 0 && (
          <p className="tt-muted tt-small">
            {t('settings.storage.workspaceQuota', { limit: f.bytes(m.quota.workspace) })}
          </p>
        )}
      </section>
    </div>
  )
}

function SpaceRow({ space, own }: { space: SpaceUsage; own?: boolean }) {
  const t = useT()
  const f = useFormat()
  const name = own
    ? t('settings.storage.privateYours')
    : space.owner
      ? t('settings.storage.privateOf', { name: space.owner.name })
      : space.name
  return (
    <li className="tt-row">
      <div className="tt-row-main">
        <strong>{name}</strong>
        {space.owner && !own && <span className="tt-muted tt-small"> {space.owner.email}</span>}
        <span className="tt-muted tt-small">
          {t('settings.storage.breakdown', {
            documents: f.bytes(space.knowledge_versioned),
            files: f.bytes(space.knowledge_unversioned),
            photos: f.bytes(space.attachments),
          })}
        </span>
        <Meter
          label={name}
          used={space.total}
          limit={space.limit}
          text={
            space.limit > 0
              ? t('settings.storage.usedOf', { used: f.bytes(space.total), limit: f.bytes(space.limit) })
              : f.bytes(space.total)
          }
        />
      </div>
    </li>
  )
}

// A figure with a bar when there is a limit to measure it against.
function Meter({ label, used, limit, text }: { label: string; used: number; limit: number; text: string }) {
  return (
    <div className="tt-storage-meter">
      <span>{text}</span>
      {limit > 0 && <progress value={Math.min(used, limit)} max={limit} aria-label={label} />}
    </div>
  )
}

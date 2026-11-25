// Storage: what the workspace's spaces store, against the quotas the operator set
// (docs/quotas.md). Admins see every space, the members' private ones by their owner's name;
// a member sees their own private space and nothing of anyone else's, which the server
// enforces. A quota of 0 is none.

import { api, type MyStorage, type SpaceUsage, type StorageReport, usagePath } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Badge } from '../ui/Badge'
import { EmptyState } from '../ui/EmptyState'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Alert, Loading, Section, SettingsTab } from './parts'

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
    <SettingsTab title={t('settings.storage.title')} help={t('settings.storage.intro')}>
      <Alert error={report.error} />
      {!r && !report.error && <Loading />}
      {r && (
        <Panel padded className="settings-panel-body">
          <div className="settings-storage-total">
            <span className="settings-person-name">
              {t('settings.storage.workspace')}
              <Fullness used={r.total} limit={r.quota.workspace} />
            </span>
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
          </div>
          <p className="settings-muted">
            {r.quota.user > 0
              ? t('settings.storage.userQuota', { limit: f.bytes(r.quota.user) })
              : t('settings.storage.userNoQuota')}
          </p>
        </Panel>
      )}
      {r && (
        <Section title={t('settings.storage.spaces')}>
          <Panel>
            <Rows label={t('settings.storage.spaces')}>
              {spaces.map((s) => (
                <SpaceRow key={s.id} space={s} />
              ))}
            </Rows>
          </Panel>
        </Section>
      )}
    </SettingsTab>
  )
}

function MemberStorage({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const mine = useLoad(() => api<MyStorage>(`${usagePath(workspace)}/me`), [workspace])
  const m = mine.data
  return (
    <SettingsTab title={t('settings.storage.title')} help={t('settings.storage.yoursIntro')}>
      <Alert error={mine.error} />
      {!m && !mine.error && <Loading />}
      {m && !m.space && <EmptyState title={t('settings.storage.noPrivate')} />}
      {m?.space && (
        <Panel>
          <Rows>
            <SpaceRow space={m.space} own />
          </Rows>
        </Panel>
      )}
      {m && m.quota.workspace > 0 && (
        <p className="settings-muted">{t('settings.storage.workspaceQuota', { limit: f.bytes(m.quota.workspace) })}</p>
      )}
    </SettingsTab>
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
    <Row
      title={name}
      status={<Fullness used={space.total} limit={space.limit} />}
      detail={
        <span className="settings-row-extra">
          {space.owner && !own && <span>{space.owner.email}</span>}
          <span>
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
        </span>
      }
    />
  )
}

// A space at or near its limit says so: nearly full from nine tenths, full at the limit.
function Fullness({ used, limit }: { used: number; limit: number }) {
  const t = useT()
  if (limit <= 0) return null
  if (used >= limit) return <Badge tone="danger">{t('settings.storage.full')}</Badge>
  if (used >= limit * 0.9) return <Badge tone="warning">{t('settings.storage.nearlyFull')}</Badge>
  return null
}

// A figure with a bar when there is a limit to measure it against.
function Meter({ label, used, limit, text }: { label: string; used: number; limit: number; text: string }) {
  return (
    <span className="settings-meter">
      <span>{text}</span>
      {limit > 0 && <progress value={Math.min(used, limit)} max={limit} aria-label={label} />}
    </span>
  )
}

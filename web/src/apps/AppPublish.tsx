import { type ReactNode, useEffect, useEffectEvent, useId, useRef, useState } from 'react'
import {
  ApiError,
  type App,
  type AppActionKind,
  type AppGrant,
  type AppRole,
  api,
  errorText,
  type SchemaChange,
  type Space,
  type SpaceMember,
} from '../api'
import { byPlace } from '../byPlace'
import { has, type Key, t as translate, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { spaceLabel } from '../spaces'
import { Button } from '../ui/Button'
import { Checkbox } from '../ui/Checkbox'
import { Dialog } from '../ui/Dialog'
import { Heading } from '../ui/Heading'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { ApproveAndPublishDialog, SlotsSection, undecided, usable, useSlots } from './AppIntegrations'
import { useIsAdmin } from './admin'
import { effectLines } from './effectLines'

export function appError(e: unknown, name?: string): string {
  if (e instanceof ApiError) {
    const capability = e.params?.capability
    if (e.code === 'app_forbidden' && typeof capability === 'string') {
      const key = `apps.needs.${capability}`
      if (has(key)) return translate(key as Key)
    }
    if (e.code === 'app_changed') return translate('apps.changed', { name: name ?? '' })
  }
  return errorText(e)
}

export async function publishDraft(
  base: string,
  version: number,
  acknowledge: SchemaChange[],
): Promise<{ published: App } | { changes: SchemaChange[] }> {
  try {
    const published = await api<App>(`${base}/publish`, { method: 'POST', body: { version, acknowledge } })
    return { published }
  } catch (e) {
    if (e instanceof ApiError && e.code === 'app_migration_required') {
      const changes = (e.body as { params?: { changes?: SchemaChange[] } }).params?.changes
      if (changes && changes.length > 0) return { changes }
    }
    throw e
  }
}

export function MigrationDialog({
  changes,
  busy,
  error,
  onPublish,
  onCancel,
}: {
  changes: SchemaChange[]
  busy: boolean
  error?: string | null
  onPublish: () => void
  onCancel: () => void
}) {
  const t = useT()
  const [understood, setUnderstood] = useState(false)
  return (
    <Dialog
      open
      onClose={onCancel}
      dismissible={!busy}
      title={t('apps.migration.title')}
      description={t('apps.migration.intro')}
      actions={
        <>
          <Button disabled={busy} onClick={onCancel}>
            {t('common.cancel')}
          </Button>
          <Button variant="primary" loading={busy} disabled={!understood} onClick={onPublish}>
            {t('apps.migration.publish')}
          </Button>
        </>
      }
    >
      <ul className="apps-list">
        {changes.map((c) => {
          const key = `apps.migration.${c.change}`
          return (
            <li key={`${c.change}-${c.field}`}>
              {has(key) ? t(key as Key, { field: c.field }) : `${c.change}: ${c.field}`}
            </li>
          )
        })}
      </ul>
      <Checkbox
        checked={understood}
        onChange={(e) => setUnderstood(e.target.checked)}
        label={t('apps.migration.confirm')}
      />
      {error && <Notice tone="danger">{error}</Notice>}
    </Dialog>
  )
}

const ROLE_ORDER: AppRole[] = ['builder', 'publisher', 'user', 'data_manager', 'reader']

function Audience({ app, space, spaceBase, base }: { app: App; space: Space; spaceBase: string; base: string }) {
  const t = useT()
  const named = space.kind === 'shared' && app.access.restricted && app.access.manage_roles
  const roles = useLoad(
    () =>
      named
        ? Promise.all([
            api<{ grants: AppGrant[] }>(`${base}/roles`),
            api<{ members: SpaceMember[] }>(`${spaceBase}/members`),
          ])
        : Promise.resolve(null),
    [named, base, spaceBase],
  )
  if (space.kind === 'private') return <p>{t('apps.builder.audience.private')}</p>
  if (!app.access.restricted) return <p>{t('apps.builder.audience.space', { space: spaceLabel(space) })}</p>
  if (!roles.data) {
    if (!named || roles.error) return <p>{t('apps.builder.audience.restricted')}</p>
    return (
      <div role="status" aria-busy="true">
        <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
        <Skeleton width="60%" />
      </div>
    )
  }
  const [{ grants }, { members }] = roles.data
  const line = (name: string, held: AppRole[]) =>
    t('apps.builder.audience.person', {
      name,
      roles: ROLE_ORDER.filter((r) => held.includes(r))
        .map((r) => t(`apps.roles.${r}`))
        .join(', '),
    })
  return (
    <>
      <p>{t('apps.builder.audience.restricted')}</p>
      <ul className="apps-list">
        {grants.map((g) => {
          const name =
            g.user_id === null
              ? t('apps.roles.everyoneElse')
              : (members.find((m) => m.id === g.user_id)?.name ?? t('apps.records.unknownUser'))
          return <li key={g.user_id ?? 'default'}>{line(name, g.roles)}</li>
        })}
      </ul>
    </>
  )
}

const CHANGES: Partial<Record<AppActionKind, Key>> = {
  insert: 'apps.builder.can.insert',
  update: 'apps.builder.can.update',
  delete: 'apps.builder.can.delete',
}

function Capabilities({ app }: { app: App }) {
  const t = useT()
  const f = useFormat()
  if (!app.draft) return <p className="apps-note">{t('apps.templates.noReview')}</p>
  const fields = app.draft?.collection?.fields ?? []
  const kinds = new Set(Object.values(app.draft?.actions ?? {}).map((a) => a.kind))
  const changes = (Object.keys(CHANGES) as AppActionKind[]).filter((k) => kinds.has(k))
  const effects = effectLines(app, app.draft, f)
  return (
    <>
      {fields.length > 0 && (
        <p>
          {t('apps.builder.can.read', {
            fields: f.list(
              fields.map((fl) => t('apps.builder.field', { name: fl.name, type: t(`apps.fieldType.${fl.type}`) })),
            ),
          })}
        </p>
      )}
      {changes.length > 0 ? (
        <ul className="apps-list">
          {changes.map((k) => (
            <li key={k}>{t(CHANGES[k] as Key)}</li>
          ))}
        </ul>
      ) : (
        <p>{t('apps.builder.can.readOnly')}</p>
      )}
      {effects.length > 0 ? (
        <>
          <p>{t('apps.builder.can.effectsIntro')}</p>
          <ul className="apps-list">
            {byPlace(effects).map(({ key, item: line }) => (
              <li key={key}>{line}</li>
            ))}
          </ul>
          <p className="apps-note">{t('apps.builder.can.effectsAudit')}</p>
        </>
      ) : (
        <p className="apps-note">{t('apps.builder.can.nothingElse')}</p>
      )}
    </>
  )
}

export function PublishPanel({
  base,
  spaceBase,
  app,
  space,
  onChanged,
  inDialog = false,
  headingLevel = 3,
}: {
  base: string
  spaceBase: string
  app: App
  space: Space
  onChanged: () => void
  inDialog?: boolean
  headingLevel?: 2 | 3
}) {
  const t = useT()
  const f = useFormat()
  const admin = useIsAdmin()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const slots = useSlots(base, app)
  const declares = Object.keys(app.draft?.integrations ?? {}).length > 0
  const blocked = (slots.data?.slots ?? []).filter((s) => !usable(s))
  const toApprove = blocked.filter(undecided)
  const broken = blocked.filter((s) => !undecided(s))
  const gated = declares && (blocked.length > 0 || slots.data === undefined)
  const [approving, setApproving] = useState<number | null>(null)
  const [reviewed, setReviewed] = useState<number | null>(null)
  const confirming = reviewed !== null
  const [migration, setMigration] = useState<{ changes: SchemaChange[]; version: number } | null>(null)
  const [published, setPublished] = useState<number | null>(null)
  const [moved, setMoved] = useState(false)
  const [granted, setGranted] = useState<string[]>([])
  const standing = granted.filter((name) =>
    (slots.data?.slots ?? []).some((s) => s.slot === name && s.status === 'approved'),
  )
  const seen = useRef(app.version)
  const current = useRef(app.version)
  current.current = app.version
  const actionButton = useRef<HTMLButtonElement>(null)
  const [refocus, setRefocus] = useState(0)
  useEffect(() => {
    if (refocus > 0) actionButton.current?.focus()
  }, [refocus])
  const confirmTextId = useId()
  const draftVersionSeen = useEffectEvent((version: number) => {
    if (seen.current === version) return
    seen.current = version
    if (reviewed !== null || migration !== null || approving !== null) {
      setReviewed(null)
      setMigration(null)
      setApproving(null)
      setMoved(true)
    }
  })
  useEffect(() => draftVersionSeen(app.version), [app.version])
  const personal = space.kind === 'private'
  const mayPublish = space.access === 'write' && app.access.publish
  const nothing = app.live !== null && !app.unpublished_changes

  const run = async (version: number, acknowledge: SchemaChange[]): Promise<string | null> => {
    setBusy(true)
    setError(null)
    setMoved(false)
    try {
      const result = await publishDraft(base, version, acknowledge)
      if ('changes' in result) {
        setMigration({ changes: result.changes, version })
      } else {
        setMigration(null)
        setReviewed(null)
        setGranted([])
        setPublished(result.published.live?.number ?? null)
        onChanged()
      }
      return null
    } catch (e) {
      const message = appError(e, app.name)
      setError(message)
      if (acknowledge.length === 0) {
        setReviewed(null)
        setMigration(null)
      }
      onChanged()
      return message
    } finally {
      setBusy(false)
    }
  }

  const confirmText = t('apps.builder.publish.confirmShared', { space: spaceLabel(space) })
  const confirmButton = (
    <Button
      ref={inDialog ? actionButton : undefined}
      variant="primary"
      loading={busy}
      aria-describedby={inDialog ? confirmTextId : undefined}
      onClick={() => run(reviewed ?? app.version, [])}
    >
      {t('apps.builder.publish.confirm', { space: spaceLabel(space) })}
    </Button>
  )
  const cancelConfirm = () => {
    if (!busy) setReviewed(null)
  }

  let action: ReactNode = null
  if (app.needs_board) action = <p className="apps-note">{t('apps.templates.publishNeedsBoard')}</p>
  else if (!mayPublish)
    action = (
      <p className="apps-note">
        {t(space.access === 'write' ? 'apps.builder.publish.needsPublisher' : 'apps.builder.publish.readOnly')}
      </p>
    )
  else if (migration) action = null
  else if (nothing) action = published === null && <p className="apps-note">{t('apps.builder.publish.nothing')}</p>
  else if (gated)
    action =
      blocked.length === 0 ? null : admin && broken.length === 0 ? (
        <div className="apps-actions">
          <Button ref={actionButton} variant="primary" onClick={() => setApproving(app.version)}>
            {t('apps.integrations.approveAndPublish')}
          </Button>
        </div>
      ) : (
        <p className="apps-note">
          {t(broken.length > 0 ? 'apps.integrations.publishBroken' : 'apps.integrations.publishWaits', {
            slots: f.list(blocked.map((s) => s.slot)),
          })}
        </p>
      )
  else if (personal)
    action = (
      <div className="apps-actions">
        <Button ref={actionButton} variant="primary" loading={busy} onClick={() => run(app.version, [])}>
          {t('apps.builder.publish.now')}
        </Button>
      </div>
    )
  else if (confirming && inDialog)
    action = (
      <div className="apps-stack">
        <p id={confirmTextId}>{confirmText}</p>
        <div className="apps-actions">
          {confirmButton}
          <Button
            disabled={busy}
            onClick={() => {
              cancelConfirm()
              setRefocus((n) => n + 1)
            }}
          >
            {t('common.cancel')}
          </Button>
        </div>
      </div>
    )
  else
    action = (
      <div className="apps-actions">
        <Button
          ref={actionButton}
          variant="primary"
          disabled={confirming}
          onClick={() => {
            setMoved(false)
            setReviewed(app.version)
            if (inDialog) setRefocus((n) => n + 1)
          }}
        >
          {t('apps.builder.publish.review')}
        </Button>
      </div>
    )

  const body = (
    <>
      <dl className="apps-summary">
        <dt>{t('apps.builder.audience.title')}</dt>
        <dd>
          <Audience app={app} space={space} spaceBase={spaceBase} base={base} />
        </dd>
        <dt>{t('apps.builder.can.title')}</dt>
        <dd>
          <Capabilities app={app} />
        </dd>
        {declares && (
          <>
            <dt>{t('apps.integrations.title')}</dt>
            <dd>
              <SlotsSection
                workspace={space.workspace_id}
                base={base}
                slots={slots.data}
                error={slots.error}
                mayRequest={mayPublish && !admin}
                admin={admin}
                reload={slots.reload}
              />
            </dd>
          </>
        )}
      </dl>
      {published !== null && (
        <Notice tone="success">
          {t(personal ? 'apps.builder.publish.donePersonal' : 'apps.builder.publish.doneShared', {
            number: published,
            space: spaceLabel(space),
          })}
        </Notice>
      )}
      {error && !migration && <Notice tone="danger">{error}</Notice>}
      {moved && <Notice tone="danger">{t('apps.builder.publish.moved')}</Notice>}
      {standing.length > 0 && (
        <Notice tone="warning">{t('apps.integrations.approve.kept', { slots: f.list(standing) })}</Notice>
      )}
      {action}
      {migration && mayPublish && (
        <MigrationDialog
          changes={migration.changes}
          busy={busy}
          error={error}
          onPublish={() => run(migration.version, migration.changes)}
          onCancel={() => {
            setMigration(null)
            setError(null)
            setRefocus((n) => n + 1)
          }}
        />
      )}
      {approving !== null && admin && mayPublish && !migration && (
        <ApproveAndPublishDialog
          workspace={space.workspace_id}
          base={base}
          space={space}
          slots={toApprove}
          onClose={() => {
            if (busy) return
            setApproving(null)
            setRefocus((n) => n + 1)
          }}
          onApproval={(slot) => {
            setGranted((g) => (g.includes(slot) ? g : [...g, slot]))
            slots.reload()
          }}
          onFailed={(done) => {
            slots.reload()
            setGranted(done)
          }}
          onContractChanged={(done) => {
            setApproving(null)
            setMoved(true)
            setGranted(done)
            onChanged()
            slots.reload()
            setRefocus((n) => n + 1)
          }}
          onApproved={async (done) => {
            const version = approving
            slots.reload()
            if (current.current !== version) {
              setGranted(done)
              return null
            }
            setGranted(done)
            const refused = await run(version, [])
            slots.reload()
            if (refused !== null) return refused
            setApproving(null)
            return null
          }}
        />
      )}
      {confirming && !inDialog && !migration && (
        <Dialog
          open
          onClose={cancelConfirm}
          title={t('apps.builder.publish.confirm', { space: spaceLabel(space) })}
          size="sm"
          actions={
            <>
              <Button disabled={busy} onClick={cancelConfirm} data-autofocus>
                {t('common.cancel')}
              </Button>
              {confirmButton}
            </>
          }
        >
          <p>{confirmText}</p>
        </Dialog>
      )}
    </>
  )

  if (inDialog) return <div className="apps-stack">{body}</div>
  return (
    <Panel as="section" padded aria-labelledby="apps-publish-title" className="apps-stack">
      <Heading level={headingLevel} id="apps-publish-title">
        {t('apps.builder.publish.title')}
      </Heading>
      {body}
    </Panel>
  )
}

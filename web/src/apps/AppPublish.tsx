// Publishing an app's draft, for the Apps page and the builder: the server's refusals said in the
// user's language, the acknowledgement of the schema changes a publish may need, and, in the
// builder, a panel that says before anything goes live who will be able to use the app and what it
// can read and change. The server decides every one of these (the app's roles, the space's, what
// must be acknowledged); nothing here widens what it allows.

import { useEffect, useRef, useState } from 'react'
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
import { has, type Key, t as translate, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { spaceLabel } from '../spaces'

// What went wrong with a change to an app, for people. A role that doesn't allow it names what is
// missing (the capability the server says: design, publish, …), and a stale version says the app
// moved on; anything else is the server's code, translated, as everywhere.
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

// The result of asking the server to publish the draft at version, the one the person saw: the
// app, or the schema changes it wants acknowledged first (nothing is published then). A draft
// that has moved on is the server's `app_changed`, thrown like any other refusal.
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

// The changes of the data's schema a publish waits for the person to acknowledge: each said in
// words, and the publish only once they have said they understand.
export function MigrationNotice({
  changes,
  busy,
  onPublish,
  onCancel,
}: {
  changes: SchemaChange[]
  busy: boolean
  onPublish: () => void
  onCancel: () => void
}) {
  const t = useT()
  const [understood, setUnderstood] = useState(false)
  return (
    <fieldset className="tt-app-migration">
      <legend>
        <strong>{t('apps.migration.title')}</strong>
      </legend>
      <p className="tt-muted tt-small">{t('apps.migration.intro')}</p>
      <ul>
        {changes.map((c) => {
          const key = `apps.migration.${c.change}`
          return (
            <li key={`${c.change}-${c.field}`}>
              {has(key) ? t(key as Key, { field: c.field }) : `${c.change}: ${c.field}`}
            </li>
          )
        })}
      </ul>
      <label className="tt-inline-field">
        <input type="checkbox" checked={understood} onChange={(e) => setUnderstood(e.target.checked)} />
        {t('apps.migration.confirm')}
      </label>
      <div className="tt-app-actions">
        <button type="button" className="tt-button tt-primary" disabled={busy || !understood} onClick={onPublish}>
          {t('apps.migration.publish')}
        </button>
        <button type="button" className="tt-button" disabled={busy} onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </fieldset>
  )
}

const ROLE_ORDER: AppRole[] = ['builder', 'publisher', 'user', 'data_manager', 'reader']

// Who will be able to use the app once it is live, from what the server says: the space it is in
// and, when roles are set and the caller may read them, the people who hold them.
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
    return <p>{t(named && !roles.error ? 'app.loading' : 'apps.builder.audience.restricted')}</p>
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
      <ul>
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

// What the app can read and change, from its draft: the fields of its records and what its buttons
// do to them. An app runs nothing else: it reaches no other app, document or account.
function Capabilities({ app }: { app: App }) {
  const t = useT()
  const f = useFormat()
  const fields = app.draft?.collection?.fields ?? []
  const kinds = new Set(Object.values(app.draft?.actions ?? {}).map((a) => a.kind))
  const changes = (Object.keys(CHANGES) as AppActionKind[]).filter((k) => kinds.has(k))
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
        <ul>
          {changes.map((k) => (
            <li key={k}>{t(CHANGES[k] as Key)}</li>
          ))}
        </ul>
      ) : (
        <p>{t('apps.builder.can.readOnly')}</p>
      )}
      <p className="tt-muted tt-small">{t('apps.builder.can.nothingElse')}</p>
    </>
  )
}

// Publishing from the builder. In a private space it is one step; in a shared one it is a second
// step that names who it goes to, and only someone with the publisher role has it at all.
export function PublishPanel({
  base,
  spaceBase,
  app,
  space,
  onChanged,
}: {
  // …/spaces/{space}/apps/{app}, and …/spaces/{space}
  base: string
  spaceBase: string
  app: App
  space: Space
  // The app changed (published, or someone else changed it): the caller reads it again.
  onChanged: () => void
}) {
  const t = useT()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // The draft's version the person is confirming, which a publish names: the draft they were
  // shown, never a newer one that arrived while the confirmation was open.
  const [reviewed, setReviewed] = useState<number | null>(null)
  const confirming = reviewed !== null
  const [migration, setMigration] = useState<{ changes: SchemaChange[]; version: number } | null>(null)
  const [published, setPublished] = useState<number | null>(null)
  // The draft changed under a confirmation: it was closed, and the person confirms again.
  const [moved, setMoved] = useState(false)
  const seen = useRef(app.version)
  // biome-ignore lint/correctness/useExhaustiveDependencies: when the draft's version changes
  useEffect(() => {
    if (seen.current === app.version) return
    seen.current = app.version
    if (reviewed !== null || migration !== null) {
      setReviewed(null)
      setMigration(null)
      setMoved(true)
    }
  }, [app.version])
  const personal = space.kind === 'private'
  const mayPublish = space.access === 'write' && app.access.publish
  const nothing = app.live !== null && !app.unpublished_changes

  // version is the draft the person reviewed (a personal app has no review: the one on screen).
  const run = async (version: number, acknowledge: SchemaChange[]) => {
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
        setPublished(result.published.live?.number ?? null)
        onChanged()
      }
    } catch (e) {
      setError(appError(e, app.name))
      setReviewed(null)
      onChanged()
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="tt-publish" aria-labelledby="tt-publish-title">
      <h3 id="tt-publish-title">{t('apps.builder.publish.title')}</h3>
      <dl className="tt-publish-summary">
        <dt>{t('apps.builder.audience.title')}</dt>
        <dd>
          <Audience app={app} space={space} spaceBase={spaceBase} base={base} />
        </dd>
        <dt>{t('apps.builder.can.title')}</dt>
        <dd>
          <Capabilities app={app} />
        </dd>
      </dl>
      {published !== null && (
        <p role="status" className="tt-published">
          {t(personal ? 'apps.builder.publish.donePersonal' : 'apps.builder.publish.doneShared', {
            number: published,
            space: spaceLabel(space),
          })}
        </p>
      )}
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {moved && (
        <p role="alert" className="tt-error">
          {t('apps.builder.publish.moved')}
        </p>
      )}
      {!mayPublish ? (
        <p className="tt-muted tt-small">
          {t(space.access === 'write' ? 'apps.builder.publish.needsPublisher' : 'apps.builder.publish.readOnly')}
        </p>
      ) : migration ? (
        <MigrationNotice
          changes={migration.changes}
          busy={busy}
          onPublish={() => run(migration.version, migration.changes)}
          onCancel={() => setMigration(null)}
        />
      ) : nothing ? (
        published === null && <p className="tt-muted tt-small">{t('apps.builder.publish.nothing')}</p>
      ) : personal || confirming ? (
        <div className="tt-app-actions">
          {!personal && (
            <p className="tt-small">{t('apps.builder.publish.confirmShared', { space: spaceLabel(space) })}</p>
          )}
          <button
            type="button"
            className="tt-button tt-primary"
            disabled={busy}
            onClick={() => run(reviewed ?? app.version, [])}
          >
            {personal ? t('apps.builder.publish.now') : t('apps.builder.publish.confirm', { space: spaceLabel(space) })}
          </button>
          {!personal && (
            <button type="button" className="tt-button" disabled={busy} onClick={() => setReviewed(null)}>
              {t('common.cancel')}
            </button>
          )}
        </div>
      ) : (
        <button
          type="button"
          className="tt-button tt-primary"
          onClick={() => {
            setMoved(false)
            setReviewed(app.version)
          }}
        >
          {t('apps.builder.publish.review')}
        </button>
      )}
    </section>
  )
}

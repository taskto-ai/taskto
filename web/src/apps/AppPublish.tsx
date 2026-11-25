// Publishing an app's draft, for the Apps page and the builder: the server's refusals said in the
// user's language, the acknowledgement of the schema changes a publish may need (a dialog that says
// what changes for stored records), and a panel that says before anything goes live who will be
// able to use the app and what it can read and change. The server decides every one of these (the app's roles, the space's, what
// must be acknowledged); nothing here widens what it allows.

import { type ReactNode, useEffect, useId, useRef, useState } from 'react'
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

// The changes of the data's schema a publish waits for the person to acknowledge, in a dialog: each
// said in words, and the publish only once they have said they understand.
export function MigrationDialog({
  changes,
  busy,
  error,
  onPublish,
  onCancel,
}: {
  changes: SchemaChange[]
  busy: boolean
  // What went wrong with the last try: the dialog stays, for another.
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
      // While it saves, Escape and the close button do nothing (the library's guard).
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
    if (!named || roles.error) return <p>{t('apps.builder.audience.restricted')}</p>
    // Who may use it is still on its way: the shape of the line, and a word for screen readers.
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

// What the app can read and change, from its draft: the fields of its records, what its buttons do
// to them and what their effects do outside them (a card on a board, a document, a place to open),
// each said with where it goes. Only when it has no effects does it say that it reaches nothing else:
// an app runs nothing but those, so it never reaches another app or an account.
function Capabilities({ app }: { app: App }) {
  const t = useT()
  const f = useFormat()
  // Without the draft (the caller may not design or publish the app) there is nothing to say: an empty
  // list of effects would read as an app that reaches nothing else.
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
            {effects.map((line, i) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: the lines are the draft's effects in their order
              <li key={i}>{line}</li>
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

// Publishing, from the builder, an installed draft or the Apps page. What goes live is said first:
// who will use it and what it can read and change. In a private space publishing is one step; in a
// shared one a dialog names who it goes to first, and only someone with the publisher role has it
// at all. A publish that changes how stored records are treated asks for that in a dialog too.
// inDialog: the panel is itself the body of a dialog (the Apps page's), so its confirmation is
// drawn in it rather than in a second one.
export function PublishPanel({
  base,
  spaceBase,
  app,
  space,
  onChanged,
  inDialog = false,
  headingLevel = 3,
}: {
  // …/spaces/{space}/apps/{app}, and …/spaces/{space}
  base: string
  spaceBase: string
  app: App
  space: Space
  // The app changed (published, or someone else changed it): the caller reads it again.
  onChanged: () => void
  inDialog?: boolean
  // Where the panel's heading sits in the page's outline.
  headingLevel?: 2 | 3
}) {
  const t = useT()
  const f = useFormat()
  const admin = useIsAdmin()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // The outside services the draft asks for: a release can't be made of it until each is approved.
  const slots = useSlots(base, app)
  const declares = Object.keys(app.draft?.integrations ?? {}).length > 0
  const blocked = (slots.data?.slots ?? []).filter((s) => !usable(s))
  const toApprove = blocked.filter(undecided)
  // Approved, but the integration was switched off or taken out of the space since: an approval can't
  // mend that, an admin does in Settings.
  const broken = blocked.filter((s) => !undecided(s))
  const gated = declares && (blocked.length > 0 || (slots.data === undefined && !slots.error))
  // The draft's version an admin is approving and publishing at once, as `reviewed` is for a review.
  const [approving, setApproving] = useState<number | null>(null)
  // The draft's version the person is confirming, which a publish names: the draft they were
  // shown, never a newer one that arrived while the confirmation was open.
  const [reviewed, setReviewed] = useState<number | null>(null)
  const confirming = reviewed !== null
  const [migration, setMigration] = useState<{ changes: SchemaChange[]; version: number } | null>(null)
  const [published, setPublished] = useState<number | null>(null)
  // The draft changed under a confirmation: it was closed, and the person confirms again.
  const [moved, setMoved] = useState(false)
  const seen = useRef(app.version)
  // The step's main button, and the inline confirmation's: focus goes there when what it was on
  // goes away (a cancelled schema dialog, a review that became its confirmation).
  const actionButton = useRef<HTMLButtonElement>(null)
  const [refocus, setRefocus] = useState(0)
  useEffect(() => {
    if (refocus > 0) actionButton.current?.focus()
  }, [refocus])
  const confirmTextId = useId()
  // biome-ignore lint/correctness/useExhaustiveDependencies: when the draft's version changes
  useEffect(() => {
    if (seen.current === app.version) return
    seen.current = app.version
    if (reviewed !== null || migration !== null || approving !== null) {
      setReviewed(null)
      setMigration(null)
      setApproving(null)
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
      // A refusal after the acknowledgement keeps the schema dialog open, with the error in it, for
      // another try; any other closes the step.
      if (acknowledge.length === 0) {
        setReviewed(null)
        setMigration(null)
      }
      onChanged()
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
    // What is asked of an outside service has no approval yet: publishing is refused until it has.
    // An admin approves and publishes in one step; anyone else waits for one (the section above asks).
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
    // Already in a dialog: the confirmation is its next step, in place.
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
            // In a dialog, the button gives way to the confirmation, which takes the focus.
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
          onApproved={async () => {
            slots.reload()
            await run(approving, [])
            slots.reload()
            setApproving(null)
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

// Building an app by asking the agent: you describe it, choose where it lives (your private space,
// unless you pick a shared one), and the agent makes a draft with sample data in a conversation.
// On a wide screen the conversation is beside a live preview of the draft, across the whole width;
// on a phone Build and Preview are two panes of which one shows, chosen with a switch, and both stay
// mounted, so neither loses its place (the conversation its scroll and message, the preview its
// form) when you switch. What the
// preview does is simulated on sample data and changes nothing real (docs/api.md, …/preview);
// publishing is a separate, plain step with who can use the app and what it can read and change.
// "Edit with AI" is the same, started on an existing app: the agent changes its draft, and the
// published release keeps working until someone publishes.
//
// What is kept in this browser (the message not sent yet, the app being built, the pane a phone
// showed) is a convenience only; the conversation and the draft are on the server.

import { type FormEvent, useCallback, useEffect, useState } from 'react'
import { ApiError, type App, api, type Conversation, type Space, ws } from '../api'
import { ConversationView } from '../conversations/ConversationView'
import type { History } from '../conversations/history'
import type { AppRef } from '../conversations/transcript'
import type { EventSourceFactory } from '../conversations/useConversationStream'
import { browserLocale, useT } from '../i18n'
import { useLoad } from '../load'
import { NARROW, useMediaQuery } from '../media'
import { useMounted } from '../mounted'
import { Link, navigate, routePath } from '../router'
import { steps } from '../session'
import { spaceLabel } from '../spaces'
import { Badge } from '../ui/Badge'
import { Button, ButtonLink } from '../ui/Button'
import { Field } from '../ui/Field'
import { Heading } from '../ui/Heading'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { SegmentedControl } from '../ui/SegmentedControl'
import { Textarea } from '../ui/Textarea'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { draftKey, readDraft, readDraftJson, writeDraft } from '../uiState'
import { appError, PublishPanel } from './AppPublish'
import { AppStage } from './AppStage'
import { LoadingRows } from './AppsLoading'
import { BackToApps, BoardChooser } from './AppTemplates'
import { appPath, type BuilderRoute, builderPath, templatesPath } from './route'
import { type Where, WhereChoice, writableSpaces } from './WhereChoice'
import './Apps.css'

// What a builder session knows: where the app lives and which app it is, once the agent has saved
// one, and `since`, the newest entry the conversation had when the building began: only an app
// the agent saved after it is this build's (a conversation may have saved others before, and the
// builder never takes one of those for the app being built). An app that was chosen, to edit it,
// is in the session from the start. It is kept in this browser as a convenience, and the page holds
// it in its own state, so a browser that keeps nothing builds all the same.
type Session = { space: number; app?: number; since?: number }

// These keys belong to the signed-in user (uiState.draftKey).
const sessionKey = (workspace: number, conversation: number) => draftKey('builder', workspace, conversation)
const lastKey = (workspace: number) => draftKey('builder', workspace, 'last')
const paneKey = (workspace: number, conversation: number) => draftKey('builder-pane', workspace, conversation)

// The conversation a person was last building in, to continue it from the Apps page.
export function lastBuilderConversation(workspace: number): number | null {
  const n = Number(readDraft(lastKey(workspace)))
  return Number.isSafeInteger(n) && n > 0 && readDraftJson<Session>(sessionKey(workspace, n)) ? n : null
}

export function AppBuilder({
  workspace,
  route,
  streams,
}: {
  workspace: number
  route: BuilderRoute
  streams?: EventSourceFactory
}) {
  const conversation = route.kind === 'conversation' ? route.conversation : null
  // The sessions started on this page, whatever the browser kept.
  const [started, setStarted] = useState<Record<number, Session>>({})
  const session = conversation
    ? (started[conversation] ?? readDraftJson<Session>(sessionKey(workspace, conversation)))
    : null
  if (conversation && session) {
    return (
      <BuilderSession
        key={conversation}
        workspace={workspace}
        conversation={conversation}
        initial={session}
        streams={streams}
      />
    )
  }
  return (
    <BuilderStart
      key={JSON.stringify(route)}
      workspace={workspace}
      route={route}
      onStarted={(id, next) => {
        setStarted((all) => ({ ...all, [id]: next }))
        // Building in the conversation the page is already on has no address to go to: the session
        // is what changes.
        if (!(route.kind === 'conversation' && route.conversation === id)) {
          navigate(builderPath(workspace, { kind: 'conversation', conversation: id }))
        }
      }}
    />
  )
}

// The start: what the app should do, and where it lives.
function BuilderStart({
  workspace,
  route,
  onStarted,
}: {
  workspace: number
  route: BuilderRoute
  onStarted: (conversation: number, session: Session) => void
}) {
  const t = useT()
  const editing = route.kind === 'edit' ? route : null
  const conversation = route.kind === 'conversation' ? route.conversation : null
  const scope = editing ? `edit-${editing.space}-${editing.app}` : conversation ? `build-${conversation}` : 'new'
  const textKey = draftKey('builder-start', workspace, scope)
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  const target = useLoad(
    () => (editing ? api<App>(`${ws(workspace)}/spaces/${editing.space}/apps/${editing.app}`) : Promise.resolve(null)),
    [workspace, editing?.space, editing?.app],
  )
  const [description, setDescription] = useState(() => readDraft(textKey) ?? '')
  // biome-ignore lint/correctness/useExhaustiveDependencies: written when the text changes
  useEffect(() => writeDraft(textKey, description.trim() ? description : null), [description])
  const [where, setWhere] = useState<Where>('private')
  const [shared, setShared] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const { mine, shared: sharedSpaces } = writableSpaces(spaces.data)
  const chosen = editing
    ? spaces.data?.find((s) => s.id === editing.space)
    : where === 'private'
      ? mine
      : sharedSpaces.find((s) => String(s.id) === shared)
  const mayEdit = !editing || (target.data?.access.design && chosen?.access === 'write')
  // Its steps go on only while the page is there and the session is the one that began them: one
  // that answers after the person left, logged out, or someone else signed in, sends nothing more
  // (not their prompt, not its title), keeps no draft and opens nothing.
  const mounted = useMounted()

  const start = async (e: FormEvent) => {
    e.preventDefault()
    const content = description.trim()
    if (!content || !chosen || busy) return
    setBusy(true)
    setError(null)
    const s = steps(mounted)
    try {
      const id =
        conversation ??
        s.check(await api<Conversation>(`${ws(workspace)}/conversations`, { method: 'POST', body: {} })).id
      // Where this build begins: what the conversation already holds (it may have saved other apps
      // before) isn't part of it. A conversation made here is empty.
      const since = conversation
        ? (s.check(await api<History>(`${ws(workspace)}/conversations/${id}/entries`)).entries.at(-1)?.id ?? 0)
        : 0
      const app = target.data ?? undefined
      const place = t(chosen.kind === 'private' ? 'apps.builder.prompt.private' : 'apps.builder.prompt.shared', {
        space: chosen.name,
        id: String(chosen.id),
      })
      const prompt = app
        ? t('apps.builder.prompt.edit', {
            name: app.name,
            app: String(app.id),
            space: String(chosen.id),
            description: content,
          })
        : t('apps.builder.prompt.build', { where: place, description: content })
      s.check(
        await api(`${ws(workspace)}/conversations/${id}/messages`, {
          method: 'POST',
          body: {
            content: prompt,
            request_id: crypto.randomUUID?.() ?? `${Date.now()}`,
            browser_locale: browserLocale(),
          },
        }),
      )
      // The title is a nicety: the conversation is there either way.
      if (!conversation) {
        const title = (app?.name ?? content).replace(/\s+/g, ' ').slice(0, 60)
        s.check(
          await api(`${ws(workspace)}/conversations/${id}`, { method: 'PATCH', body: { title } }).catch(
            () => undefined,
          ),
        )
      }
      writeDraft(textKey, null)
      const session: Session = { space: chosen.id, since, ...(app ? { app: app.id } : {}) }
      writeDraft(sessionKey(workspace, id), JSON.stringify(session))
      writeDraft(lastKey(workspace), String(id))
      onStarted(id, session)
    } catch (err) {
      if (s.ok()) setError(appError(err))
    } finally {
      if (s.ok()) setBusy(false)
    }
  }

  return (
    <Page width="reading">
      <PageHeader
        path={<BackToApps workspace={workspace} />}
        title={t(editing ? 'apps.builder.editTitle' : 'apps.builder.title')}
        help={t(editing ? 'apps.builder.editIntro' : 'apps.builder.intro')}
        actions={!editing && <ButtonLink href={templatesPath(workspace)}>{t('apps.templates.start')}</ButtonLink>}
      />
      {(spaces.error || target.error) && <Notice tone="danger">{spaces.error ?? target.error}</Notice>}
      {editing && target.data && (
        <p>{t('apps.builder.editing', { name: target.data.name, space: chosen ? spaceLabel(chosen) : '…' })}</p>
      )}
      {editing && target.data && !mayEdit && (
        <Notice tone="danger">
          {appError(new ApiError(403, '', { code: 'app_forbidden', params: { capability: 'design' } }))}
        </Notice>
      )}
      <form className="apps-form" onSubmit={start}>
        <Field label={t(editing ? 'apps.builder.whatChange' : 'apps.builder.what')}>
          <Textarea
            rows={5}
            value={description}
            maxLength={4000}
            required
            placeholder={t(editing ? 'apps.builder.changePlaceholder' : 'apps.builder.placeholder')}
            onChange={(e) => setDescription(e.target.value)}
          />
        </Field>
        {!editing && (
          <WhereChoice spaces={spaces.data} where={where} onWhere={setWhere} shared={shared} onShared={setShared} />
        )}
        {error && <Notice tone="danger">{error}</Notice>}
        {conversation && (
          <p className="apps-note">
            {t('apps.builder.inThisConversation')}{' '}
            <Link href={routePath({ workspace, section: 'conversations', item: String(conversation) })}>
              {t('apps.builder.backToConversation')}
            </Link>
          </p>
        )}
        <div className="apps-actions">
          <Button type="submit" variant="primary" loading={busy} disabled={!description.trim() || !chosen || !mayEdit}>
            {t(editing ? 'apps.builder.startEdit' : 'apps.builder.start')}
          </Button>
        </div>
      </form>
    </Page>
  )
}

type Pane = 'build' | 'preview'

// The conversation and the preview of what it builds.
function BuilderSession({
  workspace,
  conversation,
  initial,
  streams,
}: {
  workspace: number
  conversation: number
  initial: Session
  streams?: EventSourceFactory
}) {
  const t = useT()
  const narrow = useMediaQuery(NARROW)
  const [session, setSession] = useState<Session>(initial)
  const [pane, setPane] = useState<Pane>(() =>
    readDraft(paneKey(workspace, conversation)) === 'preview' ? 'preview' : 'build',
  )
  const [save, setSave] = useState<string | null>(null)
  // The agent saved the app while the person was on the other pane.
  const [fresh, setFresh] = useState(false)
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  const base = session.app ? `${ws(workspace)}/spaces/${session.space}/apps/${session.app}` : null
  // null: the server has no such app for this user (removed, moved, or its space is no longer theirs).
  const info = useLoad(
    () =>
      base
        ? api<App>(base).catch((e: unknown) => {
            if (e instanceof ApiError && e.status === 404) return null
            throw e
          })
        : Promise.resolve(undefined),
    [base],
  )
  const reload = info.reload
  // biome-ignore lint/correctness/useExhaustiveDependencies: read again after each save, keeping what is shown
  useEffect(() => {
    if (save) reload()
  }, [save])

  // This is the conversation the Apps page offers to continue.
  useEffect(() => writeDraft(lastKey(workspace), String(conversation)), [workspace, conversation])
  const choose = (next: Pane) => {
    setPane(next)
    writeDraft(paneKey(workspace, conversation), next)
    if (next === 'preview') setFresh(false)
  }

  const saved = useCallback(
    (app: AppRef, id: string) => {
      setSession((s) => {
        const next: Session = { ...s, space: app.space, app: app.app }
        if (s.space === next.space && s.app === next.app) return s
        writeDraft(sessionKey(workspace, conversation), JSON.stringify(next))
        return next
      })
      setSave(id)
      setFresh(true)
    },
    [workspace, conversation],
  )

  return (
    <div className="apps-builder" data-narrow={narrow || undefined} data-pane={pane}>
      <header className="apps-builder-bar">
        <span className="apps-builder-back">
          <BackToApps workspace={workspace} />
        </span>
        <Heading level={1} size="title" className="apps-builder-title">
          {t('apps.builder.title')}
        </Heading>
        {info.data && (
          <span className="apps-builder-state">
            <DraftMarker app={info.data} />
          </span>
        )}
        {narrow && (
          <SegmentedControl<Pane>
            label={t('apps.builder.panes')}
            size="sm"
            className="apps-builder-switch"
            value={pane}
            onChange={choose}
            options={[
              { value: 'build', label: t('apps.builder.tab.build') },
              {
                value: 'preview',
                label: (
                  <>
                    {t('apps.builder.tab.preview')}
                    {fresh && pane === 'build' && (
                      <>
                        <span className="apps-builder-dot" aria-hidden="true" />
                        <VisuallyHidden> {t('apps.builder.tab.updated')}</VisuallyHidden>
                      </>
                    )}
                  </>
                ),
              },
            ]}
          />
        )}
      </header>
      <VisuallyHidden>
        <span role="status">
          {narrow ? t(pane === 'build' ? 'apps.builder.showingBuild' : 'apps.builder.showingPreview') : ''}
        </span>
      </VisuallyHidden>
      <div className="apps-builder-panes">
        <section
          aria-label={t('apps.builder.tab.build')}
          className="apps-builder-pane"
          data-pane="build"
          data-active={!narrow || pane === 'build'}
          inert={narrow && pane !== 'build' ? true : undefined}
        >
          <ConversationView
            workspace={workspace}
            id={conversation}
            backHref={routePath({ workspace, section: 'apps' })}
            onChanged={() => undefined}
            streams={streams}
            embedApps={false}
            onSavedApp={saved}
            savedAfter={session.since}
          />
        </section>
        <section
          aria-label={t('apps.builder.tab.preview')}
          className="apps-builder-pane"
          data-pane="preview"
          data-active={!narrow || pane === 'preview'}
          inert={narrow && pane !== 'preview' ? true : undefined}
        >
          <BuilderPreview workspace={workspace} session={session} spaces={spaces.data} base={base} info={info} />
        </section>
      </div>
    </div>
  )
}

// Where the draft being built stands: live, with changes not published yet, never published, or
// offline.
function DraftMarker({ app }: { app: App }) {
  const t = useT()
  if (app.archived_at) return <Badge tone="neutral">{t('apps.offlineBadge')}</Badge>
  if (!app.live)
    return (
      <Badge tone="warning" dot>
        {t('apps.notPublished')}
      </Badge>
    )
  if (app.unpublished_changes)
    return (
      <Badge tone="warning" dot>
        {t('apps.unpublishedChanges')}
      </Badge>
    )
  return (
    <Badge tone="success" dot>
      {t('apps.stage.live', { number: app.live.number })}
    </Badge>
  )
}

// The draft the conversation is building, and publishing it.
function BuilderPreview({
  workspace,
  session,
  spaces,
  base,
  info,
}: {
  workspace: number
  session: Session
  spaces: Space[] | undefined
  base: string | null
  // The app being built, read again after each save the agent makes.
  info: { data?: App | null; error?: string | null; reload: () => void }
}) {
  const t = useT()
  const reload = info.reload
  const space = spaces?.find((s) => s.id === session.space)

  return (
    <div className="apps-builder-preview">
      <div className="apps-builder-preview-head">
        <Heading level={2}>{t('apps.builder.tab.preview')}</Heading>
        {info.data && (
          <Link href={appPath(workspace, session.space, info.data.id)}>
            {t('apps.openNamedInApps', { name: info.data.name })}
          </Link>
        )}
      </div>
      {!base && <p className="apps-note">{t('apps.builder.preview.empty')}</p>}
      {info.error && <Notice tone="danger">{info.error}</Notice>}
      {base && info.data === undefined && !info.error && <LoadingRows count={1} />}
      {info.data === null && <p className="apps-note">{t('apps.unavailable')}</p>}
      {info.data?.archived_at && <p className="apps-note">{t('apps.offlineNamed', { name: info.data.name })}</p>}
      {info.data && !info.data.archived_at && space && base && (
        <>
          <Heading level={3} size="subsection">
            {info.data.name}
          </Heading>
          {info.data.access.design || info.data.access.publish ? (
            <AppStage key={base} base={base} app={info.data} builder />
          ) : (
            <p className="apps-note">
              {appError(new ApiError(403, '', { code: 'app_forbidden', params: { capability: 'design' } }))}
            </p>
          )}
          {info.data.needs_board && info.data.access.design && (
            <BoardChooser workspace={workspace} base={base} app={info.data} spaces={spaces} onChanged={reload} />
          )}
          <PublishPanel
            base={base}
            spaceBase={`${ws(workspace)}/spaces/${space.id}`}
            app={info.data}
            space={space}
            onChanged={reload}
          />
        </>
      )}
      {info.data && !info.data.archived_at && spaces && !space && <p className="apps-note">{t('apps.unavailable')}</p>}
    </div>
  )
}

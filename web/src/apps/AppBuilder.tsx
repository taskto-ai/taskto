// Building an app by asking the agent: you describe it, choose where it lives (your private space,
// unless you pick a shared one), and the agent makes a draft with sample data in a conversation.
// On a wide screen the conversation is beside a live preview of the draft; on a phone Build and
// Preview are two panes of which one shows, and both stay mounted, so neither loses its place
// (the conversation its scroll and message, the preview its form) when you switch. What the
// preview does is simulated on sample data and changes nothing real (docs/api.md, …/preview);
// publishing is a separate, plain step with who can use the app and what it can read and change.
// "Edit with AI" is the same, started on an existing app: the agent changes its draft, and the
// published release keeps working until someone publishes.
//
// What is kept in this browser (the message not sent yet, the app being built, the pane a phone
// showed) is a convenience only; the conversation and the draft are on the server.

import { type FormEvent, type KeyboardEvent, useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, type App, api, type Conversation, type Space, ws } from '../api'
import { ConversationView } from '../conversations/ConversationView'
import type { AppRef } from '../conversations/transcript'
import type { EventSourceFactory } from '../conversations/useConversationStream'
import { browserLocale, useT } from '../i18n'
import { useLoad } from '../load'
import { NARROW, useMediaQuery } from '../media'
import { Link, navigate, routePath } from '../router'
import { spaceLabel } from '../spaces'
import { readJson, readState, writeState } from '../uiState'
import { appError, PublishPanel } from './AppPublish'
import { AppStage } from './AppStage'
import { appPath, type BuilderRoute, builderPath } from './route'

// What a builder session remembers in this browser: where the app lives and which app it is, once
// the agent has saved one.
type Session = { space: number; app?: number }
const sessionKey = (workspace: number, conversation: number) => `taskto:builder:${workspace}:${conversation}`
const lastKey = (workspace: number) => `taskto:builder:${workspace}:last`
const paneKey = (workspace: number, conversation: number) => `taskto:builder-pane:${workspace}:${conversation}`

// The conversation a person was last building in, to continue it from the Apps page.
export function lastBuilderConversation(workspace: number): number | null {
  const n = Number(readState(lastKey(workspace)))
  return Number.isSafeInteger(n) && n > 0 && readJson<Session>(sessionKey(workspace, n)) ? n : null
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
  const session = conversation ? readJson<Session>(sessionKey(workspace, conversation)) : null
  if (conversation && session) {
    return <BuilderSession key={conversation} workspace={workspace} conversation={conversation} streams={streams} />
  }
  return <BuilderStart key={JSON.stringify(route)} workspace={workspace} route={route} />
}

// The start: what the app should do, and where it lives.
function BuilderStart({ workspace, route }: { workspace: number; route: BuilderRoute }) {
  const t = useT()
  const editing = route.kind === 'edit' ? route : null
  const conversation = route.kind === 'conversation' ? route.conversation : null
  const scope = editing ? `edit-${editing.space}-${editing.app}` : conversation ? `build-${conversation}` : 'new'
  const textKey = `taskto:builder-start:${workspace}:${scope}`
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  const target = useLoad(
    () => (editing ? api<App>(`${ws(workspace)}/spaces/${editing.space}/apps/${editing.app}`) : Promise.resolve(null)),
    [workspace, editing?.space, editing?.app],
  )
  const [description, setDescription] = useState(() => readState(textKey) ?? '')
  // biome-ignore lint/correctness/useExhaustiveDependencies: written when the text changes
  useEffect(() => writeState(textKey, description.trim() ? description : null), [description])
  const [where, setWhere] = useState<'private' | 'shared'>('private')
  const [shared, setShared] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const writable = (spaces.data ?? []).filter((s) => s.access === 'write' && !s.archived_at)
  const mine = writable.find((s) => s.kind === 'private')
  const sharedSpaces = writable.filter((s) => s.kind === 'shared')
  const chosen = editing
    ? spaces.data?.find((s) => s.id === editing.space)
    : where === 'private'
      ? mine
      : sharedSpaces.find((s) => String(s.id) === shared)
  const mayEdit = !editing || (target.data?.access.design && chosen?.access === 'write')

  const start = async (e: FormEvent) => {
    e.preventDefault()
    const content = description.trim()
    if (!content || !chosen || busy) return
    setBusy(true)
    setError(null)
    try {
      const id =
        conversation ?? (await api<Conversation>(`${ws(workspace)}/conversations`, { method: 'POST', body: {} })).id
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
      await api(`${ws(workspace)}/conversations/${id}/messages`, {
        method: 'POST',
        body: {
          content: prompt,
          request_id: crypto.randomUUID?.() ?? `${Date.now()}`,
          browser_locale: browserLocale(),
        },
      })
      // The title is a nicety: the conversation is there either way.
      if (!conversation) {
        const title = (app?.name ?? content).replace(/\s+/g, ' ').slice(0, 60)
        await api(`${ws(workspace)}/conversations/${id}`, { method: 'PATCH', body: { title } }).catch(() => undefined)
      }
      writeState(textKey, null)
      writeState(
        sessionKey(workspace, id),
        JSON.stringify({ space: chosen.id, ...(app ? { app: app.id } : {}) } satisfies Session),
      )
      writeState(lastKey(workspace), String(id))
      navigate(builderPath(workspace, { kind: 'conversation', conversation: id }))
    } catch (err) {
      setError(appError(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="tt-section tt-builder-start">
      <div className="tt-pane-head">
        <h1>{t(editing ? 'apps.builder.editTitle' : 'apps.builder.title')}</h1>
        <Link className="tt-button" href={routePath({ workspace, section: 'apps' })}>
          {t('apps.all')}
        </Link>
      </div>
      <p className="tt-muted">{t(editing ? 'apps.builder.editIntro' : 'apps.builder.intro')}</p>
      {(spaces.error || target.error) && (
        <p role="alert" className="tt-error">
          {spaces.error ?? target.error}
        </p>
      )}
      {editing && target.data && (
        <p>{t('apps.builder.editing', { name: target.data.name, space: chosen ? spaceLabel(chosen) : '…' })}</p>
      )}
      {editing && target.data && !mayEdit && (
        <p role="alert" className="tt-error">
          {appError(new ApiError(403, '', { code: 'app_forbidden', params: { capability: 'design' } }))}
        </p>
      )}
      <form className="tt-builder-form" onSubmit={start}>
        <label className="tt-field">
          <span>{t(editing ? 'apps.builder.whatChange' : 'apps.builder.what')}</span>
          <textarea
            rows={4}
            value={description}
            maxLength={4000}
            required
            placeholder={t(editing ? 'apps.builder.changePlaceholder' : 'apps.builder.placeholder')}
            onChange={(e) => setDescription(e.target.value)}
          />
        </label>
        {!editing && (
          <fieldset className="tt-builder-where">
            <legend>{t('apps.builder.where')}</legend>
            <label className="tt-inline-field">
              <input type="radio" name="where" checked={where === 'private'} onChange={() => setWhere('private')} />
              {t('apps.builder.wherePrivate')}
            </label>
            <label className="tt-inline-field">
              <input
                type="radio"
                name="where"
                checked={where === 'shared'}
                disabled={sharedSpaces.length === 0}
                onChange={() => {
                  setWhere('shared')
                  setShared((s) => s || String(sharedSpaces[0]?.id ?? ''))
                }}
              />
              {t('apps.builder.whereShared')}
            </label>
            {where === 'shared' && (
              <label className="tt-inline-field">
                {t('apps.builder.sharedSpace')}
                <select value={shared} onChange={(e) => setShared(e.target.value)} required>
                  {sharedSpaces.map((s) => (
                    <option key={s.id} value={s.id}>
                      {spaceLabel(s)}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <p className="tt-muted tt-small">
              {t(where === 'private' ? 'apps.builder.privateNote' : 'apps.builder.sharedNote')}
            </p>
            {sharedSpaces.length === 0 && spaces.data && (
              <p className="tt-muted tt-small">{t('apps.builder.noSharedSpaces')}</p>
            )}
          </fieldset>
        )}
        {error && (
          <p role="alert" className="tt-error">
            {error}
          </p>
        )}
        {conversation && (
          <p className="tt-muted tt-small">
            {t('apps.builder.inThisConversation')}{' '}
            <Link href={routePath({ workspace, section: 'conversations', item: String(conversation) })}>
              {t('apps.builder.backToConversation')}
            </Link>
          </p>
        )}
        <div className="tt-app-actions">
          <button
            type="submit"
            className="tt-button tt-primary"
            disabled={busy || !description.trim() || !chosen || !mayEdit}
          >
            {t(editing ? 'apps.builder.startEdit' : 'apps.builder.start')}
          </button>
        </div>
      </form>
    </div>
  )
}

type Pane = 'build' | 'preview'

// The conversation and the preview of what it builds.
function BuilderSession({
  workspace,
  conversation,
  streams,
}: {
  workspace: number
  conversation: number
  streams?: EventSourceFactory
}) {
  const t = useT()
  const narrow = useMediaQuery(NARROW)
  const [session, setSession] = useState<Session | null>(() => readJson<Session>(sessionKey(workspace, conversation)))
  const [pane, setPane] = useState<Pane>(() =>
    readState(paneKey(workspace, conversation)) === 'preview' ? 'preview' : 'build',
  )
  const [save, setSave] = useState<string | null>(null)
  // The agent saved the app while the person was on the other pane.
  const [fresh, setFresh] = useState(false)
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])

  // This is the conversation the Apps page offers to continue.
  useEffect(() => writeState(lastKey(workspace), String(conversation)), [workspace, conversation])
  // Where focus goes when the pane changes: into the pane for a tap or click, and stays on the tab
  // for the arrow keys, which only move along the tabs.
  const focusPanel = useRef(true)
  const choose = (next: Pane, toPanel = true) => {
    focusPanel.current = toPanel
    setPane(next)
    writeState(paneKey(workspace, conversation), next)
    if (next === 'preview') setFresh(false)
  }
  const buildPanel = useRef<HTMLElement>(null)
  const previewPanel = useRef<HTMLElement>(null)
  const tabs = { build: useRef<HTMLButtonElement>(null), preview: useRef<HTMLButtonElement>(null) }
  // Moving between panes moves focus into the one that shows, so a keyboard or screen reader user
  // isn't left on a pane that has gone.
  const moved = useRef(false)
  // biome-ignore lint/correctness/useExhaustiveDependencies: when the pane changes
  useEffect(() => {
    if (!moved.current) {
      moved.current = true
      return
    }
    if (narrow && focusPanel.current)
      (pane === 'build' ? buildPanel : previewPanel).current?.focus({ preventScroll: true })
  }, [pane])

  const saved = useCallback(
    (app: AppRef, id: string) => {
      setSession((s) => {
        const next: Session = { space: app.space, app: app.app }
        if (s && s.space === next.space && s.app === next.app) return s
        writeState(sessionKey(workspace, conversation), JSON.stringify(next))
        return next
      })
      setSave(id)
      setFresh(true)
    },
    [workspace, conversation],
  )

  const arrow = (e: KeyboardEvent) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return
    e.preventDefault()
    const next: Pane = pane === 'build' ? 'preview' : 'build'
    choose(next, false)
    tabs[next].current?.focus()
  }

  return (
    <div className="tt-builder" data-narrow={narrow || undefined} data-pane={pane}>
      <div className="tt-builder-bar">
        <h1>{t('apps.builder.title')}</h1>
        {narrow && (
          <div role="tablist" aria-label={t('apps.builder.panes')} className="tt-builder-tabs" onKeyDown={arrow}>
            <button
              ref={tabs.build}
              type="button"
              role="tab"
              id="tt-builder-tab-build"
              aria-selected={pane === 'build'}
              aria-controls="tt-builder-panel-build"
              tabIndex={pane === 'build' ? 0 : -1}
              className="tt-button"
              onClick={() => choose('build')}
            >
              {t('apps.builder.tab.build')}
            </button>
            <button
              ref={tabs.preview}
              type="button"
              role="tab"
              id="tt-builder-tab-preview"
              aria-selected={pane === 'preview'}
              aria-controls="tt-builder-panel-preview"
              tabIndex={pane === 'preview' ? 0 : -1}
              className="tt-button"
              onClick={() => choose('preview')}
            >
              {t('apps.builder.tab.preview')}
              {fresh && pane === 'build' && (
                <>
                  <span className="tt-builder-dot" aria-hidden="true" />
                  <span className="tt-sr-only"> {t('apps.builder.tab.updated')}</span>
                </>
              )}
            </button>
          </div>
        )}
      </div>
      <p className="tt-sr-only" role="status">
        {narrow ? t(pane === 'build' ? 'apps.builder.showingBuild' : 'apps.builder.showingPreview') : ''}
      </p>
      <div className="tt-builder-panes">
        <section
          ref={buildPanel}
          id="tt-builder-panel-build"
          role={narrow ? 'tabpanel' : 'region'}
          aria-labelledby={narrow ? 'tt-builder-tab-build' : undefined}
          aria-label={narrow ? undefined : t('apps.builder.tab.build')}
          tabIndex={-1}
          className="tt-builder-pane"
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
          />
        </section>
        <section
          ref={previewPanel}
          id="tt-builder-panel-preview"
          role={narrow ? 'tabpanel' : 'region'}
          aria-labelledby={narrow ? 'tt-builder-tab-preview' : undefined}
          aria-label={narrow ? undefined : t('apps.builder.tab.preview')}
          tabIndex={-1}
          className="tt-builder-pane"
          data-pane="preview"
          data-active={!narrow || pane === 'preview'}
          inert={narrow && pane !== 'preview' ? true : undefined}
        >
          <BuilderPreview workspace={workspace} session={session} spaces={spaces.data} save={save} />
        </section>
      </div>
    </div>
  )
}

// The draft the conversation is building, and publishing it.
function BuilderPreview({
  workspace,
  session,
  spaces,
  save,
}: {
  workspace: number
  session: Session | null
  spaces: Space[] | undefined
  // Changes with each save the agent makes: the app is read again.
  save: string | null
}) {
  const t = useT()
  const base = session?.app ? `${ws(workspace)}/spaces/${session.space}/apps/${session.app}` : null
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
  const space = spaces?.find((s) => s.id === session?.space)

  return (
    <div className="tt-builder-preview">
      <div className="tt-pane-head">
        <h2>{t('apps.builder.tab.preview')}</h2>
        {info.data && session && (
          <Link className="tt-small" href={appPath(workspace, session.space, info.data.id)}>
            {t('apps.openNamedInApps', { name: info.data.name })}
          </Link>
        )}
      </div>
      {!base && <p className="tt-muted">{t('apps.builder.preview.empty')}</p>}
      {info.error && (
        <p role="alert" className="tt-error">
          {info.error}
        </p>
      )}
      {base && info.data === undefined && !info.error && <p className="tt-muted">{t('app.loading')}</p>}
      {info.data === null && <p className="tt-muted">{t('apps.unavailable')}</p>}
      {info.data?.archived_at && <p className="tt-muted">{t('apps.offlineNamed', { name: info.data.name })}</p>}
      {info.data && !info.data.archived_at && space && base && (
        <>
          <h3 className="tt-builder-app">{info.data.name}</h3>
          {info.data.access.design || info.data.access.publish ? (
            <AppStage key={base} base={base} app={info.data} builder />
          ) : (
            <p className="tt-muted">
              {appError(new ApiError(403, '', { code: 'app_forbidden', params: { capability: 'design' } }))}
            </p>
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
      {info.data && !info.data.archived_at && spaces && !space && <p className="tt-muted">{t('apps.unavailable')}</p>}
    </div>
  )
}

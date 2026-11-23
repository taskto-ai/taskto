// Draws one personal app: the server sends A2UI messages, button actions go back to it,
// and the messages it answers with update the screen.
//
// An app is drawn from one of its releases (or, to try it, its draft on sample data), and its buttons name the
// release they were drawn from, so the server can tell an action of an older screen from the live
// one's. A release that needs more than this client's catalog has (a component, a function, a
// newer definition format) isn't drawn: the app says the client must be updated.

import { A2uiSurface, type ReactCatalogComponent } from '@a2ui/react/v0_9'
import { MessageProcessor, STRICT_VALIDATION, type SurfaceModel } from '@a2ui/web_core/v0_9'
import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ApiError,
  type AppEffect,
  api,
  type DefinitionNeeds,
  errorText,
  type PreviewReply,
  type PreviewRow,
} from '../api'
import { t as translate, useT } from '../i18n'
import { applyUpdate, useUpdatePrompt } from '../pwa/update'
import { EffectPreview } from './AppEffects'
import { appsCatalog, unsupportedBy } from './catalog'
import { keepLabelsLinked } from './labels'
import { withOperation } from './operations'
import { type AppFiles, AppFilesContext } from './PhotoInput'
import { SubmitCaptures, SubmitCapturesContext } from './submit'
import { TextDictation, workspaceOf } from './TextDictation'

export type ActionPayload = Parameters<NonNullable<ConstructorParameters<typeof MessageProcessor>[1]>>[0]

export type AppTransport = {
  load(): Promise<unknown[]>
  act(action: ActionPayload): Promise<unknown[]>
  // Where the app's attachments are, for the photo components; left out where there is no server
  // to hold them.
  files?: AppFiles
  // In a draft's preview: what the action just run would set going on boards and documents, which
  // changes nothing (docs/api.md, …/actions/preview). Absent where actions are real.
  simulate?(action: ActionPayload): Promise<AppEffect[]>
}

// The draft's preview: its screen and its buttons work, on sample rows the server makes and this
// transport keeps (the server holds nothing of a preview and writes none of the app's rows:
// docs/api.md, …/preview). A new transport starts a new preview, so a caller keeps one while the
// preview is in use.
//
// The steps go one at a time, each on the rows the one before left: two taps in quick succession
// would otherwise both start from the same rows, and whichever answer came last would drop the
// other's change.
export function previewTransport(base: string): AppTransport {
  let rows: PreviewRow[] | undefined
  // The effects the last step would have set going: the server works them out on the preview's own
  // rows, in the step itself, so a button with none costs no request and no real record is read.
  let effects: AppEffect[] = []
  let queue: Promise<unknown> = Promise.resolve()
  const step = (action?: ActionPayload, fresh = false): Promise<unknown[]> => {
    const run = queue.then(async () => {
      // Loading starts over from the sample rows.
      if (fresh) rows = undefined
      const reply = await api<PreviewReply>(`${base}/preview`, {
        method: 'POST',
        body: action && rows ? { rows, action } : {},
      })
      rows = reply.rows
      effects = reply.effects ?? []
      return reply.messages
    })
    // A step that failed leaves the rows as they were, and the next goes on from them.
    queue = run.catch(() => undefined)
    return run
  }
  return {
    load: () => step(undefined, true),
    act: (action) => step(action),
    simulate: async () => effects,
  }
}

// The app's routes on the server: base is …/spaces/{space}/apps/{app}. release is the live
// release's number, which the screen is drawn from and its actions name; 'draft' is the draft's
// preview, whose actions run on sample rows (previewTransport) and never on the app's own. screen opens an app
// with screens on one of them (a pin's); widget draws one of the live release's Home widgets instead (a
// bounded view whose actions are the app's own, answered with the rows it holds).
//
// Every action carries an operation, its idempotency key (see operations.ts).
export function httpTransport(
  base: string,
  release?: number | 'draft',
  opts: { screen?: string; widget?: string } = {},
): AppTransport {
  const query = (params: Record<string, string | number | undefined>) => {
    const q = new URLSearchParams()
    for (const [key, value] of Object.entries(params)) if (value !== undefined) q.set(key, String(value))
    const text = q.toString()
    return text ? `?${text}` : ''
  }
  // The draft is its preview, on sample rows (previewTransport), and opens on its entry screen.
  if (release === 'draft') return previewTransport(base)
  const scope = `${base}|${release ?? 'live'}|${opts.widget ?? ''}`
  const post = (path: string, action: ActionPayload) =>
    withOperation(scope, action, (operation) =>
      api<unknown[]>(path, {
        method: 'POST',
        body: release === undefined ? { ...action, operation } : { ...action, release, operation },
      }),
    )
  if (opts.widget !== undefined) {
    return {
      load: () => api<unknown[]>(`${base}/surface${query({ widget: opts.widget })}`),
      act: (action) => post(`${base}/actions${query({ widget: opts.widget })}`, action),
    }
  }
  return {
    files: { base, uploads: true },
    load: () => api<unknown[]>(`${base}/surface${query({ release, screen: opts.screen })}`),
    act: (action) => post(`${base}/actions`, action),
  }
}

// The code of the server's answer to a change made against a record that has moved on.
const RECORD_CONFLICT = 'app_record_conflict'

export function AppView({
  surfaceId,
  label,
  transport,
  release,
  reloadKey = 0,
  onAction,
}: {
  surfaceId: string
  label: string
  transport: AppTransport
  // What the definition drawn needs, when known: checked against this client's catalog first.
  release?: DefinitionNeeds
  // Changed by the caller when the app's data changed behind the screen's back (a record was
  // restored): the screen is loaded again.
  reloadKey?: number
  // Called after an action ran, which may have set effects going.
  onAction?: () => void
}) {
  const [error, setError] = useState<string | null>(null)
  // What the last button of a draft's preview would have set going.
  const [simulated, setSimulated] = useState<AppEffect[] | null>(null)
  const onActionRef = useRef(onAction)
  onActionRef.current = onAction
  const [surface, setSurface] = useState<SurfaceModel<ReactCatalogComponent> | null>(null)
  // Read through a ref so a caller passing a new transport object on each render doesn't
  // reload the app: only a different surface does.
  const transportRef = useRef(transport)
  transportRef.current = transport
  const t = useT()
  const unsupported = release ? unsupportedBy(release) : []
  const blocked = unsupported.length > 0
  const root = useRef<HTMLElement | null>(null)
  // The section, once drawn, for what looks at the screen's own elements (the labels, the dictation).
  const [section, setSection] = useState<HTMLElement | null>(null)
  const attach = useCallback((el: HTMLElement | null) => {
    root.current = el
    setSection(el)
  }, [])
  // What the screen's inputs do as a form is submitted, before its action is sent (a location taken
  // then, submit.ts).
  const captures = useRef(new SubmitCaptures())
  const dictation = transport.files?.uploads ? workspaceOf(transport.files.base) : null
  // A submission is being made (a location taken as the form is submitted, the request on its way):
  // its buttons are off until it has an answer, and a press that gets through (a key) is that submission.
  const [busy, setBusy] = useState(false)
  useEffect(() => captures.current.subscribe(() => setBusy(captures.current.busy)), [])
  useEffect(() => {
    if (!busy || !section) return
    const off = [...section.querySelectorAll<HTMLButtonElement>('button:not(:disabled)')].filter(
      (b) => !b.closest('.tt-capture'),
    )
    for (const b of off) b.disabled = true
    return () => {
      for (const b of off) b.disabled = false
    }
  }, [busy, section])

  // The renderer's inputs aren't linked to their labels: name them for assistive technology.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the section exists once the screen is drawn
  useEffect(() => (root.current ? keepLabelsLinked(root.current) : undefined), [blocked])

  // biome-ignore lint/correctness/useExhaustiveDependencies: a new reloadKey is a reload
  useEffect(() => {
    if (blocked) return
    let live = true
    // Declared before the processor whose action handler calls it.
    const apply = (messages: unknown[]) => {
      const errors = processor.processMessagesWithErrors(messages)
      if (errors.length > 0) throw new Error(translate('apps.invalidScreen', { error: JSON.stringify(errors[0]) }))
      if (live) setSurface(processor.model.surfacesMap.get(surfaceId) ?? null)
    }
    // An update or a delete says which revision of its record the person saw (the item's
    // _revision), so the server refuses it if someone changed the record since.
    const withRevision = (action: ActionPayload): ActionPayload => {
      const id = Number((action.context as { id?: unknown } | undefined)?.id)
      if (!Number.isInteger(id) || id < 1) return action
      const items: unknown = processor.model.surfacesMap.get(surfaceId)?.dataModel.get('/items')
      const item = Array.isArray(items) ? items.find((i) => (i as { id?: unknown })?.id === id) : undefined
      const revision = (item as { _revision?: unknown } | undefined)?._revision
      return typeof revision === 'number' ? { ...action, expectedRevision: revision } : action
    }
    const processor = new MessageProcessor<ReactCatalogComponent>(
      [appsCatalog],
      (action) =>
        // One submission at a time: a second press while the first is capturing or sending is that one.
        captures.current.once(action, async () => {
          try {
            const prepared = await captures.current.apply(action)
            if (prepared.block) {
              setError(prepared.block)
              return
            }
            const pressed = withRevision({ ...action, context: prepared.context ?? action.context })
            apply(await transportRef.current.act(pressed))
            setError(null)
            // A preview says what the button would have set going; a live app's effects are listed by the page.
            setSimulated(transportRef.current.simulate ? await transportRef.current.simulate(pressed) : null)
            onActionRef.current?.()
          } catch (e) {
            setSimulated(null)
            if (e instanceof ApiError && (e.code === RECORD_CONFLICT || e.code === 'offline_record_changed')) {
              // Show the record as it is now, so the next try is made against its latest revision.
              try {
                apply(await transportRef.current.load())
              } catch {
                // The conflict is what the person is told; the screen stays as it was.
              }
            }
            setError(errorText(e))
          }
        }),
      // Without this the processor accepts components outside the catalog and silently draws
      // nothing; strict refuses them, and dangling references, with an error. The server
      // checked the app when it was saved too.
      { validationConfig: STRICT_VALIDATION },
    )
    transportRef.current
      .load()
      .then(apply)
      .catch((e: unknown) => live && setError(errorText(e)))
    return () => {
      live = false
    }
  }, [surfaceId, blocked, reloadKey])

  if (blocked) return <UpdateRequired label={label} unsupported={unsupported} />

  return (
    <section className="tt-app" aria-label={label} aria-busy={busy || undefined} ref={attach}>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <AppFilesContext.Provider value={transport.files ?? null}>
        <SubmitCapturesContext.Provider value={captures.current}>
          {surface ? <A2uiSurface surface={surface} /> : !error && <p className="tt-muted">{t('app.loading')}</p>}
        </SubmitCapturesContext.Provider>
      </AppFilesContext.Provider>
      {dictation !== null && surface && <TextDictation root={section} workspace={dictation} />}
      {simulated && <EffectPreview effects={simulated} />}
    </section>
  )
}

// Shown instead of an app this client can't draw: the person updates TaskTo.AI (the waiting
// version if there is one, else a reload), and the app is there.
function UpdateRequired({ label, unsupported }: { label: string; unsupported: string[] }) {
  const t = useT()
  const waiting = useUpdatePrompt()
  return (
    <section className="tt-app tt-app-update" aria-label={label}>
      <p role="status">
        <strong>{t('apps.updateRequired.title')}</strong>
      </p>
      <p className="tt-muted">{t('apps.updateRequired.message')}</p>
      <p className="tt-muted tt-small">{t('apps.updateRequired.needs', { needs: unsupported.join(', ') })}</p>
      <button
        type="button"
        className="tt-button tt-primary"
        onClick={() => (waiting ? applyUpdate() : window.location.reload())}
      >
        {t('apps.updateRequired.update')}
      </button>
    </section>
  )
}

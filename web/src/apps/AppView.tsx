// Draws one personal app: the server sends A2UI messages, button actions go back to it,
// and the messages it answers with update the screen.
//
// An app is drawn from one of its releases (or, to preview it, its draft), and its buttons name the
// release they were drawn from, so the server can tell an action of an older screen from the live
// one's. A release that needs more than this client's catalog has (a component, a function, a
// newer definition format) isn't drawn: the app says the client must be updated.

import { A2uiSurface, type ReactCatalogComponent } from '@a2ui/react/v0_9'
import { MessageProcessor, STRICT_VALIDATION, type SurfaceModel } from '@a2ui/web_core/v0_9'
import { useEffect, useRef, useState } from 'react'
import { ApiError, type AppEffect, api, type DefinitionNeeds, errorText } from '../api'
import { t as translate, useT } from '../i18n'
import { applyUpdate, useUpdatePrompt } from '../pwa/update'
import { EffectPreview } from './AppEffects'
import { appsCatalog, unsupportedBy } from './catalog'
import { keepLabelsLinked } from './labels'
import { type AppFiles, AppFilesContext } from './PhotoInput'

export type ActionPayload = Parameters<NonNullable<ConstructorParameters<typeof MessageProcessor>[1]>>[0]

export type AppTransport = {
  load(): Promise<unknown[]>
  act(action: ActionPayload): Promise<unknown[]>
  // Where the app's attachments are, for the photo components; left out where there is no server
  // to hold them.
  files?: AppFiles
}

// What a draft's button does in a preview: nothing is saved, and the answer is the effects it would
// set going, simulated, which the view shows.
export class PreviewResult extends Error {
  constructor(readonly effects: AppEffect[]) {
    super('preview')
  }
}

// A new idempotency key for a press of a button (the server's `operation`): 8 to 64 letters, digits,
// - and _.
function newOperation(): string {
  return globalThis.crypto?.randomUUID?.() ?? `op-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`
}

// What identifies one submission of a button: the action as the person sent it, without the
// renderer's per-click timestamp, which differs on every press of the same pending submission.
export function submissionKey(action: ActionPayload): string {
  const { timestamp: _timestamp, ...rest } = action as ActionPayload & { timestamp?: unknown }
  return JSON.stringify(rest)
}

// Makes the idempotency key (the server's `operation`) of each action sent. A submission gets one
// key, made once; every press of the same submission while no answer has come (the connection
// dropped, so the person can't know whether it was made) sends that key again, so the server
// changes nothing a second time and queues no second card. Any answer, even a refusal, ends the
// submission: the next press is a new one. The renderer's own timestamp doesn't make a press a
// different submission.
export function operationTracker(make: () => string = newOperation) {
  let unanswered: { sent: string; operation: string } | null = null
  return async function withOperation<T>(action: ActionPayload, send: (operation: string) => Promise<T>): Promise<T> {
    const sent = submissionKey(action)
    const operation = unanswered?.sent === sent ? unanswered.operation : make()
    unanswered = { sent, operation }
    try {
      const answer = await send(operation)
      unanswered = null
      return answer
    } catch (e) {
      // An answer, even a refusal, is the end of it; no answer is not.
      if (e instanceof ApiError) unanswered = null
      throw e
    }
  }
}

// The app's routes on the server: base is …/spaces/{space}/apps/{app}. release is the live
// release's number, which the screen is drawn from and its actions name; 'draft' draws the draft, a
// preview whose actions run on nothing: the server says what they would set going. screen opens an
// app with screens on one of them (a pin's); widget draws one of the live release's Home widgets
// instead (a bounded view whose actions are the app's own, answered with the rows it holds).
//
// Every action carries an operation, its idempotency key (see operationTracker).
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
  if (release === 'draft') {
    return {
      load: () => api<unknown[]>(`${base}/surface${query({ release: 'draft', screen: opts.screen })}`),
      act: async (action) => {
        const preview = await api<{ effects: AppEffect[] }>(`${base}/actions/preview`, { method: 'POST', body: action })
        throw new PreviewResult(preview.effects)
      },
      files: { base, uploads: false },
    }
  }
  const withOperation = operationTracker()
  const post = (path: string, action: ActionPayload) =>
    withOperation(action, (operation) =>
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
  const root = useRef<HTMLElement>(null)

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
      async (action) => {
        try {
          apply(await transportRef.current.act(withRevision(action)))
          setError(null)
          setSimulated(null)
          onActionRef.current?.()
        } catch (e) {
          if (e instanceof PreviewResult) {
            setSimulated(e.effects)
            setError(null)
            return
          }
          setSimulated(null)
          if (e instanceof ApiError && e.code === RECORD_CONFLICT) {
            // Show the record as it is now, so the next try is made against its latest revision.
            try {
              apply(await transportRef.current.load())
            } catch {
              // The conflict is what the person is told; the screen stays as it was.
            }
          }
          setError(errorText(e))
        }
      },
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
    <section className="tt-app" aria-label={label} ref={root}>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <AppFilesContext.Provider value={transport.files ?? null}>
        {surface ? <A2uiSurface surface={surface} /> : !error && <p className="tt-muted">{t('app.loading')}</p>}
      </AppFilesContext.Provider>
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

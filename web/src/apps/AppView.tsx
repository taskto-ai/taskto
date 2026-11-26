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
import { ApiError, type AppEffect, type DefinitionNeeds, errorText } from '../api'
import { t as translate, useT } from '../i18n'
import { applyUpdate, useUpdatePrompt } from '../pwa/update'
import { Button } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { EffectPreview } from './AppEffects'
import { appsCatalog, unsupportedBy } from './catalog'
import { fieldLabels, keepLabelsLinked } from './labels'
import { AppFilesContext } from './PhotoInput'
import { SubmitCaptures, SubmitCapturesContext } from './submit'
import { TextDictation, workspaceOf } from './TextDictation'
import './AppLoading.css'
import './AppView.css'
import type { ActionPayload, AppTransport } from './transport'

// The code of the server's answer to a change made against a record that has moved on.
const RECORD_CONFLICT = 'app_record_conflict'

export function AppView({
  surfaceId,
  label,
  transport,
  release,
  reloadKey = 0,
  onAction,
  frame = 'panel',
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
  // How the screen is set on the page: on a panel of its own (an app's page, a conversation), or
  // bare inside something that is already one (a Home widget).
  frame?: 'panel' | 'bare'
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
  // Only the capture fields that can be part of it (a location, a signature, a barcode: each marked
  // data-submit-free) keep theirs; a photo's are off, since removing one would delete an upload the
  // request names.
  const [busy, setBusy] = useState(false)
  useEffect(() => captures.current.subscribe(() => setBusy(captures.current.busy)), [])
  useEffect(() => {
    if (!busy || !section) return
    const off = [...section.querySelectorAll<HTMLButtonElement>('button:not(:disabled)')].filter(
      (b) => !b.closest('[data-submit-free]'),
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
            const open = processor.model.surfacesMap.get(surfaceId)
            setError(
              errorText(
                e,
                open ? fieldLabels(open.componentsModel.entries, (path) => open.dataModel.get(path)) : undefined,
              ),
            )
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

  if (blocked) return <UpdateRequired label={label} unsupported={unsupported} frame={frame} />

  return (
    <section className="ui-app" data-frame={frame} aria-label={label} aria-busy={busy || undefined} ref={attach}>
      {error && <Notice tone="danger">{error}</Notice>}
      <AppFilesContext.Provider value={transport.files ?? null}>
        <SubmitCapturesContext.Provider value={captures.current}>
          {surface ? <A2uiSurface surface={surface} /> : !error && <AppLoading />}
        </SubmitCapturesContext.Provider>
      </AppFilesContext.Provider>
      {dictation !== null && surface && <TextDictation root={section} workspace={dictation} />}
      {simulated && <EffectPreview effects={simulated} />}
    </section>
  )
}

// Shown instead of an app this client can't draw: the person updates TaskTo.AI (the waiting
// version if there is one, else a reload), and the app is there.
function UpdateRequired({
  label,
  unsupported,
  frame,
}: {
  label: string
  unsupported: string[]
  frame: 'panel' | 'bare'
}) {
  const t = useT()
  const waiting = useUpdatePrompt()
  return (
    <section className="ui-app" data-frame={frame} aria-label={label}>
      <Notice
        tone="warning"
        title={t('apps.updateRequired.title')}
        action={
          <Button variant="primary" onClick={() => (waiting ? applyUpdate() : window.location.reload())}>
            {t('apps.updateRequired.update')}
          </Button>
        }
      >
        <p>{t('apps.updateRequired.message')}</p>
        <p className="ui-app-needs">{t('apps.updateRequired.needs', { needs: unsupported.join(', ') })}</p>
      </Notice>
    </section>
  )
}

// While the screen is on its way: the shape of a form (a heading, two fields, a button), and the
// word for those who don't see it.
function AppLoading() {
  const t = useT()
  return (
    <div className="ui-app-loading" role="status">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      <Skeleton width="40%" />
      <Skeleton shape="block" height="var(--control-md)" />
      <Skeleton shape="block" height="var(--control-md)" />
      <Skeleton shape="block" width="30%" height="var(--control-md)" />
    </div>
  )
}

import { A2uiSurface, type ReactCatalogComponent } from '@a2ui/react/v0_9'
import { MessageProcessor, STRICT_VALIDATION, type SurfaceModel } from '@a2ui/web_core/v0_9'
import { useEffect, useEffectEvent, useRef, useState } from 'react'
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
import { steps } from '../session'
import type { ActionPayload, AppTransport } from './transport'

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
  release?: DefinitionNeeds
  reloadKey?: number
  onAction?: () => void
  frame?: 'panel' | 'bare'
}) {
  const [error, setError] = useState<string | null>(null)
  const [simulated, setSimulated] = useState<AppEffect[] | null>(null)
  const onActionRef = useRef(onAction)
  onActionRef.current = onAction
  const [surface, setSurface] = useState<SurfaceModel<ReactCatalogComponent> | null>(null)
  const transportRef = useRef(transport)
  transportRef.current = transport
  const unsupported = release ? unsupportedBy(release) : []
  const blocked = unsupported.length > 0
  const [section, setSection] = useState<HTMLElement | null>(null)
  const captures = useRef(new SubmitCaptures())
  const dictation = transport.files?.uploads ? workspaceOf(transport.files.base) : null
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

  useEffect(() => (section ? keepLabelsLinked(section) : undefined), [section])

  const startSurface = (surfaceId: string) => {
    let live = true
    const apply = (messages: unknown[]) => {
      const errors = processor.processMessagesWithErrors(messages)
      if (errors.length > 0) throw new Error(translate('apps.invalidScreen', { error: JSON.stringify(errors[0]) }))
      if (live) setSurface(processor.model.surfacesMap.get(surfaceId) ?? null)
    }
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
        captures.current.once(action, async () => {
          const s = steps()
          try {
            const prepared = await captures.current.apply(action)
            if (!s.ok()) return
            if (prepared.block) {
              setError(prepared.block)
              return
            }
            const pressed = withRevision({ ...action, context: prepared.context ?? action.context })
            const answer = await transportRef.current.act(pressed)
            if (!s.ok()) return
            apply(answer)
            setError(null)
            setSimulated(transportRef.current.simulate ? await transportRef.current.simulate(pressed) : null)
            onActionRef.current?.()
          } catch (e) {
            if (!s.ok()) return
            setSimulated(null)
            if (e instanceof ApiError && (e.code === RECORD_CONFLICT || e.code === 'offline_record_changed')) {
              try {
                apply(await transportRef.current.load())
              } catch {}
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
      { validationConfig: STRICT_VALIDATION },
    )
    transportRef.current
      .load()
      .then(apply)
      .catch((e: unknown) => live && setError(errorText(e)))
    return () => {
      live = false
    }
  }

  if (blocked) return <UpdateRequired label={label} unsupported={unsupported} frame={frame} />

  return (
    <section className="ui-app" data-frame={frame} aria-label={label} aria-busy={busy || undefined} ref={setSection}>
      <SurfaceSession key={reloadKey} surfaceId={surfaceId} start={startSurface} />
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

function SurfaceSession({ surfaceId, start }: { surfaceId: string; start: (surfaceId: string) => () => void }) {
  const run = useEffectEvent(start)
  useEffect(() => run(surfaceId), [surfaceId])
  return null
}

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

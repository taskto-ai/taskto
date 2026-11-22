// Draws one personal app: the server sends A2UI messages, button actions go back to it,
// and the messages it answers with update the screen.

import { A2uiSurface, type ReactCatalogComponent } from '@a2ui/react/v0_9'
import { MessageProcessor, STRICT_VALIDATION, type SurfaceModel } from '@a2ui/web_core/v0_9'
import { useEffect, useRef, useState } from 'react'
import { api, errorText } from '../api'
import { t as translate, useT } from '../i18n'
import { appsCatalog } from './catalog'

export type ActionPayload = Parameters<NonNullable<ConstructorParameters<typeof MessageProcessor>[1]>>[0]

export type AppTransport = {
  load(): Promise<unknown[]>
  act(action: ActionPayload): Promise<unknown[]>
}

// The app's routes on the server: base is …/spaces/{space}/apps/{app}.
export function httpTransport(base: string): AppTransport {
  return {
    load: () => api<unknown[]>(`${base}/surface`),
    act: (action) => api<unknown[]>(`${base}/actions`, { method: 'POST', body: action }),
  }
}

export function AppView({
  surfaceId,
  label,
  transport,
}: {
  surfaceId: string
  label: string
  transport: AppTransport
}) {
  const [error, setError] = useState<string | null>(null)
  const [surface, setSurface] = useState<SurfaceModel<ReactCatalogComponent> | null>(null)
  // Read through a ref so a caller passing a new transport object on each render doesn't
  // reload the app: only a different surface does.
  const transportRef = useRef(transport)
  transportRef.current = transport
  const t = useT()

  useEffect(() => {
    let live = true
    // Declared before the processor whose action handler calls it.
    const apply = (messages: unknown[]) => {
      const errors = processor.processMessagesWithErrors(messages)
      if (errors.length > 0) throw new Error(translate('apps.invalidScreen', { error: JSON.stringify(errors[0]) }))
      if (live) setSurface(processor.model.surfacesMap.get(surfaceId) ?? null)
    }
    const processor = new MessageProcessor<ReactCatalogComponent>(
      [appsCatalog],
      async (action) => {
        try {
          apply(await transportRef.current.act(action))
          setError(null)
        } catch (e) {
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
  }, [surfaceId])

  return (
    <section className="tt-app" aria-label={label}>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {surface ? <A2uiSurface surface={surface} /> : !error && <p className="tt-muted">{t('app.loading')}</p>}
    </section>
  )
}

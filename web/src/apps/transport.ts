import type { MessageProcessor } from '@a2ui/web_core/v0_9'
import { type AppEffect, api, type PreviewReply, type PreviewRow } from '../api'
import { withOperation } from './operations'
import type { AppFiles } from './PhotoInput'

export type ActionPayload = Parameters<NonNullable<ConstructorParameters<typeof MessageProcessor>[1]>>[0]

export type AppTransport = {
  load(): Promise<unknown[]>
  act(action: ActionPayload): Promise<unknown[]>
  files?: AppFiles
  simulate?(action: ActionPayload): Promise<AppEffect[]>
}

export function previewTransport(base: string): AppTransport {
  let rows: PreviewRow[] | undefined
  let effects: AppEffect[] = []
  let queue: Promise<unknown> = Promise.resolve()
  const step = (action?: ActionPayload, fresh = false): Promise<unknown[]> => {
    const run = queue.then(async () => {
      if (fresh) rows = undefined
      const reply = await api<PreviewReply>(`${base}/preview`, {
        method: 'POST',
        body: action && rows ? { rows, action } : {},
      })
      rows = reply.rows
      effects = reply.effects ?? []
      return reply.messages
    })
    queue = run.catch(() => undefined)
    return run
  }
  return {
    load: () => step(undefined, true),
    act: (action) => step(action),
    simulate: async () => effects,
  }
}

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

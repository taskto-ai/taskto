// How an app's screen talks to the server: its transport (load the surface, run an action), the live
// one over HTTP and a draft's preview on sample rows. Apart from the renderer (AppView.tsx), which
// takes a transport, so a page can make one without loading the renderer: Home and the Apps list
// load the renderer only when they draw an app (LazyAppView.tsx).

import type { MessageProcessor } from '@a2ui/web_core/v0_9'
import { type AppEffect, api, type PreviewReply, type PreviewRow } from '../api'
import { withOperation } from './operations'
import type { AppFiles } from './PhotoInput'

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

// noVNC, loaded at run time from its own unmodified files (vite.config.ts copies them to
// /vendor/novnc/, MPL-2.0 with its licence), never bundled into the app. Only the little of
// its RFB client the live view uses is typed here.

export type RFB = EventTarget & {
  viewOnly: boolean
  scaleViewport: boolean
  resizeSession: boolean
  focusOnClick: boolean
  background: string
  disconnect(): void
  focus(): void
}

export type RFBConstructor = new (target: HTMLElement, url: string, options?: { wsProtocols?: string[] }) => RFB

export const NOVNC_URL = '/vendor/novnc/core/rfb.js'

let loading: Promise<RFBConstructor> | null = null

// loadRFB imports noVNC's RFB client once.
export function loadRFB(): Promise<RFBConstructor> {
  loading ??= import(/* @vite-ignore */ NOVNC_URL).then((m: { default: RFBConstructor }) => m.default)
  loading.catch(() => {
    loading = null
  })
  return loading
}

// The live view's WebSocket, on this server (docs/api.md#get-apimebrowserlive).
export function liveURL(loc: Location = window.location): string {
  return `${loc.protocol === 'https:' ? 'wss' : 'ws'}://${loc.host}/api/me/browser/live`
}

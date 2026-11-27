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

export function loadRFB(): Promise<RFBConstructor> {
  loading ??= import(/* @vite-ignore */ NOVNC_URL).then((m: { default: RFBConstructor }) => m.default)
  loading.catch(() => {
    loading = null
  })
  return loading
}

export function liveURL(loc: Location = window.location): string {
  return `${loc.protocol === 'https:' ? 'wss' : 'ws'}://${loc.host}/api/me/browser/live`
}

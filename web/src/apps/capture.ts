// Capturing from the device for an app's fields (docs/api.md, "Location, signature and barcode
// fields"): where the person is, and the settings a screen reads of such a field. The inputs that
// draw these are LocationInput.tsx, SignaturePad.tsx and BarcodeInput.tsx.
//
// Location is read once, in the foreground, when the person presses a button or submits a form
// whose field says so. Nothing here follows the device: the browser's one-shot getCurrentPosition
// is the only call, and there is no watching and no background use anywhere in this app (a test
// searches the source for it).

import type { Format } from '../i18n'

// A location as the server stores it.
export type Location = {
  lat: number
  lon: number
  // Metres; the device's own reading, so a position typed by hand may have none.
  accuracy_m?: number
  captured_at: string
  source: 'device' | 'manual'
}

// What the screen's data model says of a capture field, at /_fields/<field>.
export type CaptureInfo = {
  // A location: 'button' (the default) or 'submit', taken as the form is submitted.
  capture?: 'button' | 'submit'
  // A barcode's format, '' for any.
  format?: string
  required?: boolean
  // Whether a stored value can still be changed (otherwise it is evidence).
  editable?: boolean
}

export function isLocation(v: unknown): v is Location {
  if (!v || typeof v !== 'object') return false
  const o = v as Record<string, unknown>
  return (
    typeof o.lat === 'number' &&
    typeof o.lon === 'number' &&
    typeof o.captured_at === 'string' &&
    (o.source === 'device' || o.source === 'manual')
  )
}

// Why a position couldn't be had.
export type PositionFailure = 'unsupported' | 'insecure' | 'denied' | 'unavailable' | 'timeout'

export class PositionError extends Error {
  constructor(readonly reason: PositionFailure) {
    super(reason)
  }
}

// How long the browser may take to answer, in milliseconds. A prompt the person is reading counts
// from when they answer it, in browsers that wait for that.
const POSITION_TIMEOUT = 20_000

// The device's position, once. Only the one-shot call: never a subscription to changes.
export function currentPosition(): Promise<Location> {
  return new Promise((resolve, reject) => {
    const geo = typeof navigator === 'undefined' ? undefined : navigator.geolocation
    if (!geo || typeof geo.getCurrentPosition !== 'function') return reject(new PositionError('unsupported'))
    // Browsers hide the position from pages that aren't secure.
    if (typeof window !== 'undefined' && window.isSecureContext === false) return reject(new PositionError('insecure'))
    try {
      geo.getCurrentPosition(
        (pos) => {
          const { latitude, longitude, accuracy } = pos.coords
          if (!Number.isFinite(latitude) || !Number.isFinite(longitude)) return reject(new PositionError('unavailable'))
          // The device's own time for the reading, never later than this device's clock now.
          const at = Math.min(Number.isFinite(pos.timestamp) ? pos.timestamp : Date.now(), Date.now())
          resolve({
            lat: latitude,
            lon: longitude,
            ...(Number.isFinite(accuracy) && accuracy >= 0 ? { accuracy_m: Math.round(accuracy * 10) / 10 } : {}),
            captured_at: new Date(at).toISOString(),
            source: 'device',
          })
        },
        (err) => reject(new PositionError(err.code === 1 ? 'denied' : err.code === 3 ? 'timeout' : 'unavailable')),
        { enableHighAccuracy: true, timeout: POSITION_TIMEOUT, maximumAge: 0 },
      )
    } catch {
      reject(new PositionError('unavailable'))
    }
  })
}

// A coordinate as people type it: "38.7223", or "38,7223" where the comma is the decimal mark.
export function parseCoordinate(text: string, limit: number): number | null {
  const s = text.trim().replace(/\s+/g, '')
  if (!/^[+-]?\d+([.,]\d+)?$/.test(s)) return null
  const n = Number(s.replace(',', '.'))
  return Number.isFinite(n) && Math.abs(n) <= limit ? n : null
}

// A position typed by hand, marked as such; null if either coordinate isn't one.
export function manualLocation(lat: string, lon: string, now = new Date()): Location | null {
  const la = parseCoordinate(lat, 90)
  const lo = parseCoordinate(lon, 180)
  return la === null || lo === null ? null : { lat: la, lon: lo, captured_at: now.toISOString(), source: 'manual' }
}

// "38.72230, -9.13930" in the person's number format.
export function coordinates(loc: Pick<Location, 'lat' | 'lon'>, f: Format): string {
  const options = { minimumFractionDigits: 5, maximumFractionDigits: 5 }
  return `${f.number(loc.lat, options)}, ${f.number(loc.lon, options)}`
}

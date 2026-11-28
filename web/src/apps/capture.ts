import type { Format } from '../i18n'

export type Location = {
  lat: number
  lon: number
  accuracy_m?: number
  captured_at: string
  source: 'device' | 'manual'
}

export type CaptureInfo = {
  capture?: 'button' | 'submit'
  format?: string
  max_length?: number
  required?: boolean
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

export type PositionFailure = 'unsupported' | 'insecure' | 'denied' | 'unavailable' | 'timeout'

export class PositionError extends Error {
  constructor(readonly reason: PositionFailure) {
    super(reason)
  }
}

const POSITION_TIMEOUT = 20_000

export function currentPosition(): Promise<Location> {
  return new Promise((resolve, reject) => {
    const geo = typeof navigator === 'undefined' ? undefined : navigator.geolocation
    if (!geo || typeof geo.getCurrentPosition !== 'function') return reject(new PositionError('unsupported'))
    if (typeof window !== 'undefined' && window.isSecureContext === false) return reject(new PositionError('insecure'))
    try {
      geo.getCurrentPosition(
        (pos) => {
          const { latitude, longitude, accuracy } = pos.coords
          if (!Number.isFinite(latitude) || !Number.isFinite(longitude)) return reject(new PositionError('unavailable'))
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

export function parseCoordinate(text: string, limit: number): number | null {
  const s = text.trim().replace(/\s+/g, '')
  if (!/^[+-]?\d+([.,]\d+)?$/.test(s)) return null
  const n = Number(s.replace(',', '.'))
  return Number.isFinite(n) && Math.abs(n) <= limit ? n : null
}

export function manualLocation(lat: string, lon: string, now = new Date()): Location | null {
  const la = parseCoordinate(lat, 90)
  const lo = parseCoordinate(lon, 180)
  return la === null || lo === null ? null : { lat: la, lon: lo, captured_at: now.toISOString(), source: 'manual' }
}

export function coordinates(loc: Pick<Location, 'lat' | 'lon'>, f: Format): string {
  const options = { minimumFractionDigits: 5, maximumFractionDigits: 5 }
  return `${f.number(loc.lat, options)}, ${f.number(loc.lon, options)}`
}

export type Detected = { rawValue: string; format?: string }

export type Detector = {
  detect(source: ImageBitmapSource): Promise<Detected[]>
}

export type Scanner = { detector: Detector; native: boolean }

type DetectorConstructor = {
  new (options?: { formats?: string[] }): Detector
  getSupportedFormats?: () => Promise<string[]>
}

export function canUseCamera(): boolean {
  return (
    typeof navigator !== 'undefined' &&
    typeof navigator.mediaDevices?.getUserMedia === 'function' &&
    (typeof window === 'undefined' || window.isSecureContext !== false)
  )
}

export async function createDetector(formats: string[] = []): Promise<Scanner> {
  const native = (globalThis as { BarcodeDetector?: DetectorConstructor }).BarcodeDetector
  if (native) {
    try {
      const supported = (await native.getSupportedFormats?.()) ?? []
      const usable = formats.length === 0 ? supported : formats.filter((f) => supported.includes(f))
      if (formats.length === 0 || usable.length === formats.length) {
        return { detector: new native(usable.length > 0 ? { formats: usable } : undefined), native: true }
      }
    } catch {}
  }
  const { BarcodeDetector } = await import('./zxing')
  const options = (formats.length > 0 ? { formats } : undefined) as ConstructorParameters<typeof BarcodeDetector>[0]
  return { detector: new BarcodeDetector(options) as Detector, native: false }
}

export const START_FAILURES = 3

export class DecoderError extends Error {
  constructor(readonly cause: unknown) {
    super(cause instanceof Error ? cause.message : String(cause))
  }
}

function isFatal(e: unknown): boolean {
  return e instanceof DOMException && (e.name === 'NotSupportedError' || e.name === 'SecurityError')
}

export type ScanSession = { result: Promise<Detected | null>; stop: () => void }

export function scan(video: HTMLVideoElement, detector: Detector, every = 200): ScanSession {
  let stopped = false
  let timer: ReturnType<typeof setTimeout> | undefined
  let finish: (d: Detected | null) => void = () => {}
  let fail: (e: unknown) => void = () => {}
  const result = new Promise<Detected | null>((resolve, reject) => {
    finish = resolve
    fail = reject
  })
  let worked = false
  let failures = 0
  const tick = async () => {
    if (stopped) return
    try {
      if (video.readyState >= 2) {
        const found = await detector.detect(video)
        worked = true
        failures = 0
        const first = found.find((d) => d.rawValue)
        if (first && !stopped) {
          stopped = true
          finish(first)
          return
        }
      }
    } catch (e) {
      failures++
      if (!stopped && (isFatal(e) || (!worked && failures >= START_FAILURES))) {
        stopped = true
        fail(new DecoderError(e))
        return
      }
    }
    if (!stopped) timer = setTimeout(tick, every)
  }
  timer = setTimeout(tick, 0)
  return {
    result,
    stop: () => {
      if (!stopped) {
        stopped = true
        finish(null)
      }
      if (timer) clearTimeout(timer)
    },
  }
}

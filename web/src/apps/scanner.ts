// Reading a barcode or QR code from the camera. The browser's own BarcodeDetector is used where it
// exists (Chrome on Android and some desktops; Safari has it behind a flag), but it isn't
// everywhere, so where it is missing the decoder comes from zxing-wasm through the
// barcode-detector package, loaded the first time it is needed: it is a separate chunk and a
// separate file, never part of the app's first load, and it is served from this server with
// the rest of the app, never fetched from a CDN. Whichever decoder is used, typed entry stays
// (BarcodeInput.tsx).

export type Detected = { rawValue: string; format?: string }

export type Detector = {
  detect(source: ImageBitmapSource): Promise<Detected[]>
}

// What createDetector found: the detector, and whether it is the browser's own.
export type Scanner = { detector: Detector; native: boolean }

type DetectorConstructor = {
  new (options?: { formats?: string[] }): Detector
  getSupportedFormats?: () => Promise<string[]>
}

// Whether the browser can open a camera at all (and the page is allowed to ask: a secure context).
export function canUseCamera(): boolean {
  return (
    typeof navigator !== 'undefined' &&
    typeof navigator.mediaDevices?.getUserMedia === 'function' &&
    (typeof window === 'undefined' || window.isSecureContext !== false)
  )
}

// A decoder for the formats asked for (any if none): the browser's when it has one that reads them,
// else the bundled one.
export async function createDetector(formats: string[] = []): Promise<Scanner> {
  const native = (globalThis as { BarcodeDetector?: DetectorConstructor }).BarcodeDetector
  if (native) {
    try {
      const supported = (await native.getSupportedFormats?.()) ?? []
      const usable = formats.length === 0 ? supported : formats.filter((f) => supported.includes(f))
      // A format the browser can't read is the bundled decoder's to read, not a reason to scan nothing.
      if (formats.length === 0 || usable.length === formats.length) {
        return { detector: new native(usable.length > 0 ? { formats: usable } : undefined), native: true }
      }
    } catch {
      // The browser's own detector didn't start: the bundled one does.
    }
  }
  const { BarcodeDetector } = await import('./zxing')
  // The formats are the field's own, one of the names the server takes: the decoder's list has them all.
  const options = (formats.length > 0 ? { formats } : undefined) as ConstructorParameters<typeof BarcodeDetector>[0]
  return { detector: new BarcodeDetector(options) as Detector, native: false }
}

// Reads the camera until a code is found, checking about five times a second so a phone isn't kept
// busy. Resolves with the code, or with null when stop() was called first.
export type ScanSession = { result: Promise<Detected | null>; stop: () => void }

export function scan(video: HTMLVideoElement, detector: Detector, every = 200): ScanSession {
  let stopped = false
  let timer: ReturnType<typeof setTimeout> | undefined
  let finish: (d: Detected | null) => void = () => {}
  const result = new Promise<Detected | null>((resolve) => {
    finish = resolve
  })
  const tick = async () => {
    if (stopped) return
    try {
      if (video.readyState >= 2) {
        const found = await detector.detect(video)
        const first = found.find((d) => d.rawValue)
        if (first && !stopped) {
          stopped = true
          finish(first)
          return
        }
      }
    } catch {
      // A frame that couldn't be read: the next one may be.
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

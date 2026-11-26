// @vitest-environment jsdom
//
// Location, signature and barcode inputs, and dictation beside text fields: each says what it can't do
// and offers the way round it (typing the coordinates, typing the name or the code, the keyboard's own
// dictation), a location is read once and never watched, and the signature pad draws without taking
// the page's scrolling away.

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, expect, test, vi } from 'vitest'
import { ApiError } from '../api'
import { forgetSharedDictationStatus } from '../conversations/Dictation'
import { beginSession, endSession } from '../session'
import { fakeFetch } from '../test/fakes'
import { AppView } from './AppView'
import { BarcodeField } from './BarcodeInput'
import { currentPosition, type Location, manualLocation, parseCoordinate } from './capture'
import { DEFINITION_VERSION } from './catalog'
import { FieldDictation } from './FieldDictation'
import { LocationField } from './LocationInput'
import type { AppFiles } from './PhotoInput'
import { SignatureField } from './SignaturePad'
import { DecoderError, START_FAILURES, scan } from './scanner'
import { SubmitCaptures, SubmitCapturesContext } from './submit'
import { insertText, workspaceOf } from './TextDictation'

const base = '/api/workspaces/1/spaces/2/apps/9'
const files: AppFiles = { base, uploads: true }
const id = 'ab'.repeat(16)

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  for (const key of ['geolocation', 'mediaDevices', 'onLine']) Reflect.deleteProperty(navigator, key)
  Reflect.deleteProperty(globalThis, 'BarcodeDetector')
  forgetSharedDictationStatus()
})

function stubNavigator(key: string, value: unknown) {
  Object.defineProperty(navigator, key, { configurable: true, value })
}

// A geolocation that answers once, or fails with a code (1 denied, 2 unavailable, 3 timeout).
function stubGeolocation(answer: { code: number } | { latitude: number; longitude: number; accuracy: number }) {
  const geo = {
    getCurrentPosition: vi.fn((ok: PositionCallback, fail?: PositionErrorCallback, _options?: PositionOptions) => {
      if ('code' in answer) fail?.({ code: answer.code, message: '' } as GeolocationPositionError)
      else ok({ coords: answer, timestamp: Date.now() - 1000 } as GeolocationPosition)
    }),
    watchPosition: vi.fn(),
    clearWatch: vi.fn(),
  }
  stubNavigator('geolocation', geo)
  return geo
}

const device: Location = {
  lat: 38.7223,
  lon: -9.1393,
  accuracy_m: 12,
  captured_at: '2026-11-20T10:00:00.000Z',
  source: 'device',
}

// ---- location -------------------------------------------------------------------------------------

function Where({
  onValue,
  info,
  readOnly,
  start,
}: {
  onValue?: (v: Location | undefined) => void
  info?: { capture?: 'button' | 'submit'; required?: boolean; editable?: boolean }
  readOnly?: boolean
  start?: Location
}) {
  const [value, setValue] = useState<Location | undefined>(start)
  return (
    <LocationField
      field="place"
      label="Where"
      value={value}
      info={info}
      readOnly={readOnly}
      onChange={(v) => {
        setValue(v)
        onValue?.(v)
      }}
    />
  )
}

test('a location is read once, on the button, and never watched', async () => {
  const geo = stubGeolocation({ latitude: 38.7223, longitude: -9.1393, accuracy: 12.34 })
  const changed = vi.fn()
  render(<Where onValue={changed} />)
  expect(geo.getCurrentPosition).not.toHaveBeenCalled() // not on its own: only when asked
  fireEvent.click(screen.getByRole('button', { name: 'Capture location' }))
  await waitFor(() => expect(changed).toHaveBeenCalledTimes(1))
  expect(geo.getCurrentPosition).toHaveBeenCalledTimes(1)
  expect(geo.watchPosition).not.toHaveBeenCalled()
  const loc = changed.mock.calls[0][0] as Location
  expect(loc).toMatchObject({ lat: 38.7223, lon: -9.1393, accuracy_m: 12.3, source: 'device' })
  expect(new Date(loc.captured_at).getTime()).toBeLessThanOrEqual(Date.now())
  // The browser was asked for a fresh reading, not an old one.
  expect(geo.getCurrentPosition.mock.calls[0][2]).toMatchObject({ maximumAge: 0 })
  // What was captured is shown with its accuracy, its time and where it came from.
  expect(await screen.findByText(/accurate to about 12 m/)).toBeTruthy()
  expect(screen.getByText(/from this device/)).toBeTruthy()
  expect(screen.getByText(/never tracked in the background/)).toBeTruthy()
})

test('a denied location says so and the coordinates can be typed, marked as typed', async () => {
  stubGeolocation({ code: 1 })
  const changed = vi.fn()
  render(<Where onValue={changed} />)
  fireEvent.click(screen.getByRole('button', { name: 'Capture location' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/Location permission was denied/)
  // The fallback is open already: type the coordinates (a comma works as the decimal mark).
  fireEvent.change(screen.getByLabelText('Latitude'), { target: { value: '38,7223' } })
  fireEvent.change(screen.getByLabelText('Longitude'), { target: { value: '-9.1393' } })
  fireEvent.click(screen.getByRole('button', { name: 'Use these coordinates' }))
  expect(changed).toHaveBeenCalledTimes(1)
  const loc = changed.mock.calls[0][0] as Location
  expect(loc).toMatchObject({ lat: 38.7223, lon: -9.1393, source: 'manual' })
  expect(loc.accuracy_m).toBeUndefined()
  expect(await screen.findByText(/entered by hand/)).toBeTruthy()
})

test('every way a position can fail has its own words', async () => {
  const cases: [Parameters<typeof stubGeolocation>[0] | 'none', RegExp][] = [
    [{ code: 2 }, /couldn’t work out its location/],
    [{ code: 3 }, /took too long/],
    ['none', /can’t share its location/],
  ]
  for (const [answer, words] of cases) {
    cleanup()
    Reflect.deleteProperty(navigator, 'geolocation')
    if (answer !== 'none') stubGeolocation(answer)
    render(<Where />)
    fireEvent.click(screen.getByRole('button', { name: 'Capture location' }))
    expect((await screen.findByRole('alert')).textContent).toMatch(words)
    expect(screen.getByLabelText('Latitude')).toBeTruthy() // the typed way is there every time
  }
})

test('an insecure page is told why there is no position', async () => {
  stubGeolocation({ latitude: 1, longitude: 1, accuracy: 1 })
  vi.stubGlobal('isSecureContext', false)
  await expect(currentPosition()).rejects.toMatchObject({ reason: 'insecure' })
})

test('typed coordinates are checked', () => {
  expect(parseCoordinate('38.7', 90)).toBe(38.7)
  expect(parseCoordinate(' -9,5 ', 180)).toBe(-9.5)
  for (const bad of ['', 'north', '91', '-90.5', '1e3', '12.', '1,2,3', 'Infinity'])
    expect(parseCoordinate(bad, 90)).toBeNull()
  expect(manualLocation('38', '181')).toBeNull()
  expect(manualLocation('38', 'x')).toBeNull()
  expect(manualLocation('38', '-9')).toMatchObject({ lat: 38, lon: -9, source: 'manual' })
})

test('typed coordinates out of range show a message and store nothing', () => {
  const changed = vi.fn()
  render(<Where onValue={changed} />)
  fireEvent.click(screen.getByRole('button', { name: 'Enter coordinates by hand' }))
  fireEvent.change(screen.getByLabelText('Latitude'), { target: { value: '120' } })
  fireEvent.change(screen.getByLabelText('Longitude'), { target: { value: '5' } })
  fireEvent.click(screen.getByRole('button', { name: 'Use these coordinates' }))
  expect(screen.getByRole('alert').textContent).toMatch(/latitude from −90 to 90/)
  expect(changed).not.toHaveBeenCalled()
  // Both inputs are described by the message, so it is read again when either is focused.
  for (const name of ['Latitude', 'Longitude']) {
    const input = screen.getByLabelText(name)
    const described = (input.getAttribute('aria-describedby') ?? '')
      .split(' ')
      .map((id) => document.getElementById(id)?.textContent ?? '')
      .join(' ')
    expect(described, name).toMatch(/latitude from −90 to 90/)
  }
})

test('a stored location is shown, read-only, with no way to change it', () => {
  render(<Where readOnly start={device} />)
  expect(screen.getByText('38.72230, -9.13930')).toBeTruthy()
  expect(screen.queryByRole('button')).toBeNull()
})

test('a location taken at submit is read when the form is submitted, once', async () => {
  const geo = stubGeolocation({ latitude: 41.15, longitude: -8.61, accuracy: 30 })
  const captures = new SubmitCaptures()
  const changed = vi.fn()
  render(
    <SubmitCapturesContext.Provider value={captures}>
      <Where onValue={changed} info={{ capture: 'submit' }} />
    </SubmitCapturesContext.Provider>,
  )
  expect(screen.getByText(/when you submit this form/)).toBeTruthy()
  expect(geo.getCurrentPosition).not.toHaveBeenCalled()
  // An action whose context doesn't pass the field doesn't read it.
  expect((await captures.apply({ name: 'add', context: { other: 1 } })).context).toEqual({ other: 1 })
  expect(geo.getCurrentPosition).not.toHaveBeenCalled()
  const sent = await captures.apply({ name: 'add', context: { place: undefined, title: 'x' } })
  expect(geo.getCurrentPosition).toHaveBeenCalledTimes(1)
  expect(sent.context).toMatchObject({ title: 'x', place: { lat: 41.15, lon: -8.61, source: 'device' } })
  // A second submit keeps the one the form has.
  await waitFor(() => expect(changed).toHaveBeenCalled())
  await captures.apply({ name: 'add', context: { place: {} } })
  expect(geo.getCurrentPosition).toHaveBeenCalledTimes(1)
  expect(geo.watchPosition).not.toHaveBeenCalled()
})

test('a location refused at submit blocks a form that needs it and not one that doesn’t', async () => {
  stubGeolocation({ code: 1 })
  const captures = new SubmitCaptures()
  const { rerender } = render(
    <SubmitCapturesContext.Provider value={captures}>
      <Where info={{ capture: 'submit', required: true }} />
    </SubmitCapturesContext.Provider>,
  )
  const blocked = await captures.apply({ name: 'add', context: { place: undefined } })
  expect(blocked.block).toMatch(/This form needs a location/)
  expect(blocked.context).toBeUndefined()
  // The reason is on the field too, and the typed way is open.
  expect((await screen.findByRole('alert')).textContent).toMatch(/denied/)
  expect(screen.getByLabelText('Latitude')).toBeTruthy()
  rerender(
    <SubmitCapturesContext.Provider value={captures}>
      <Where info={{ capture: 'submit', required: false }} />
    </SubmitCapturesContext.Provider>,
  )
  const open = await captures.apply({ name: 'add', context: { place: undefined } })
  expect(open.block).toBeUndefined()
  expect(open.context).toEqual({ place: undefined })
})

// ---- signature ------------------------------------------------------------------------------------

function stubCanvas() {
  const ctx = {
    fillRect: vi.fn(),
    beginPath: vi.fn(),
    moveTo: vi.fn(),
    lineTo: vi.fn(),
    stroke: vi.fn(),
    arc: vi.fn(),
    fill: vi.fn(),
    fillText: vi.fn(),
    measureText: vi.fn(() => ({ width: 100 })),
  }
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockImplementation(
    () => ctx as unknown as CanvasRenderingContext2D,
  )
  const blobs: string[] = []
  vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation(function (this: HTMLCanvasElement, cb, type) {
    blobs.push(String(type))
    cb(new Blob([new Uint8Array(8)], { type: type ?? 'image/png' }))
  })
  return { ctx, blobs }
}

const uploadRoutes = () => ({
  [`POST ${base}/uploads`]: {
    status: 201,
    body: {
      id,
      field: 'signed',
      status: 'uploading',
      size: 8,
      received: 0,
      attached: false,
      chunk_size: 8,
      max_chunk: 8,
    },
  },
  [`PUT ${base}/uploads/${id}?offset=0`]: { body: { id, status: 'uploading', size: 8, received: 8, attached: false } },
  [`POST ${base}/uploads/${id}/finalize`]: {
    body: { id, field: 'signed', status: 'ready', size: 8, received: 8, content_type: 'image/png', attached: false },
  },
})

function Sign({
  onValue,
  field = files,
  start,
}: {
  onValue?: (id: string | undefined) => void
  field?: AppFiles | null
  start?: string
}) {
  const [value, setValue] = useState<string | undefined>(start)
  return (
    <SignatureField
      label="Sign here"
      files={field}
      field="signed"
      value={value}
      onChange={(v) => {
        setValue(v)
        onValue?.(v)
      }}
    />
  )
}

const pad = () => screen.getByRole('img', { name: /Signature pad/ }) as HTMLCanvasElement

function drawStroke(el: HTMLElement, pointerId = 1) {
  fireEvent.pointerDown(el, { pointerId, clientX: 10, clientY: 10, button: 0, pointerType: 'touch' })
  fireEvent.pointerMove(el, { pointerId, clientX: 40, clientY: 20, pointerType: 'touch' })
  fireEvent.pointerMove(el, { pointerId, clientX: 80, clientY: 10, pointerType: 'touch' })
  fireEvent.pointerUp(el, { pointerId, clientX: 80, clientY: 10, pointerType: 'touch' })
}

test('the pad stops touch scrolling on itself only, so the page scrolls elsewhere', () => {
  stubCanvas()
  render(<Sign />)
  expect(pad().style.touchAction).toBe('none')
  // Nothing around it, up to the document, takes touches away.
  for (let el: HTMLElement | null = pad().parentElement; el; el = el.parentElement) {
    expect(el.style.touchAction, el.tagName).toBe('')
  }
  // The buttons, the name field and the rest of the component keep the default.
  for (const control of screen.getAllByRole('button')) expect(control.style.touchAction).toBe('')
})

test('touch-action: none is set on the signature pad and nowhere else in the app', () => {
  const found: string[] = []
  const walk = (dir: string) => {
    for (const name of readdirSync(dir)) {
      const path = join(dir, name)
      if (statSync(path).isDirectory()) walk(path)
      else if (/\.(tsx?|css)$/.test(name) && !/\.test\.tsx?$/.test(name)) {
        // Comments say it too; only declarations count.
        const code = readFileSync(path, 'utf8')
          .replace(/\/\*[\s\S]*?\*\//g, '')
          .replace(/^\s*\/\/.*$/gm, '')
        for (const _ of code.matchAll(/touch-action:\s*none|touchAction:\s*'none'/g)) found.push(path.split('/src/')[1])
      }
    }
  }
  walk(join(import.meta.dirname, '..'))
  // The stylesheet rule for the pad's class, and the pad's own inline style.
  expect(found.sort()).toEqual(['apps/SignaturePad.tsx', 'apps/capture.css'])
  for (const file of found.filter((f) => f.endsWith('.css'))) {
    const css = readFileSync(join(import.meta.dirname, '..', file), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')
    const selectors = [...css.matchAll(/([^{}]+)\{[^}]*touch-action:\s*none[^}]*\}/g)].map((m) => m[1].trim())
    expect(selectors.length, file).toBeGreaterThan(0)
    for (const sel of selectors) expect(sel, file).toBe('.ui-signature-canvas')
  }
})

test('drawing keeps the strokes, undo and clear take them back, and saving uploads a PNG', async () => {
  const { ctx, blobs } = stubCanvas()
  const calls = fakeFetch(uploadRoutes())
  const changed = vi.fn()
  render(<Sign onValue={changed} />)
  const save = () => screen.getByRole('button', { name: 'Save signature' }) as HTMLButtonElement
  expect(save().disabled).toBe(true) // nothing drawn yet
  drawStroke(pad(), 1)
  drawStroke(pad(), 2)
  expect(save().disabled).toBe(false)
  expect(ctx.lineTo).toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: 'Undo' }))
  expect(save().disabled).toBe(false) // one stroke is left
  fireEvent.click(screen.getByRole('button', { name: 'Undo' }))
  expect(save().disabled).toBe(true)
  drawStroke(pad())
  fireEvent.click(screen.getByRole('button', { name: 'Clear' }))
  expect(save().disabled).toBe(true)
  drawStroke(pad())
  fireEvent.click(save())
  await waitFor(() => expect(changed).toHaveBeenCalledWith(id))
  // The picture went through the photo upload: create, the bytes, finalize, for the signature field.
  expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual([
    `POST ${base}/uploads`,
    `PUT ${base}/uploads/${id}?offset=0`,
    `POST ${base}/uploads/${id}/finalize`,
  ])
  expect(calls[0].body).toEqual({ field: 'signed', size: 8 })
  expect(blobs).toEqual(['image/png'])
  // The saved signature is shown from the server's own route, and can be done again.
  const img = await screen.findByRole('img', { name: 'The saved signature' })
  expect(img.getAttribute('src')).toBe(`${base}/attachments/${id}/thumbnail`)
  expect(screen.getByRole('button', { name: 'Sign again' })).toBeTruthy()
})

test('a second finger or another pointer doesn’t draw into the stroke', () => {
  stubCanvas()
  render(<Sign />)
  fireEvent.pointerDown(pad(), { pointerId: 1, clientX: 5, clientY: 5, button: 0, pointerType: 'touch' })
  fireEvent.pointerDown(pad(), { pointerId: 2, clientX: 50, clientY: 5, button: 0, pointerType: 'touch' })
  fireEvent.pointerUp(pad(), { pointerId: 2 })
  fireEvent.pointerUp(pad(), { pointerId: 1 })
  fireEvent.click(screen.getByRole('button', { name: 'Undo' }))
  // Two pointers made one stroke, not two: one undo empties the pad.
  expect((screen.getByRole('button', { name: 'Save signature' }) as HTMLButtonElement).disabled).toBe(true)
})

test('turning the device redraws the strokes where they were', () => {
  const { ctx } = stubCanvas()
  render(<Sign />)
  drawStroke(pad())
  const before = ctx.lineTo.mock.calls.length
  fireEvent(window, new Event('orientationchange'))
  fireEvent(window, new Event('resize'))
  expect(ctx.lineTo.mock.calls.length).toBeGreaterThan(before)
})

test('a name typed and confirmed is a signature too, for the keyboard', async () => {
  const { ctx } = stubCanvas()
  fakeFetch(uploadRoutes())
  const changed = vi.fn()
  render(<Sign onValue={changed} />)
  fireEvent.click(screen.getByRole('radio', { name: 'Type your name' }))
  const save = screen.getByRole('button', { name: 'Save signature' }) as HTMLButtonElement
  expect(save.disabled).toBe(true)
  fireEvent.change(screen.getByLabelText('Your full name'), { target: { value: 'Ada Lovelace' } })
  expect(save.disabled).toBe(true) // the name alone isn't enough: it is confirmed too
  fireEvent.click(screen.getByLabelText(/typing my name here is my signature/))
  expect(save.disabled).toBe(false)
  fireEvent.click(save)
  await waitFor(() => expect(changed).toHaveBeenCalledWith(id))
  expect(ctx.fillText).toHaveBeenCalledWith('Ada Lovelace', expect.any(Number), expect.any(Number), expect.any(Number))
})

test('a signature that failed to upload keeps what was drawn and says why', async () => {
  stubCanvas()
  fakeFetch({
    [`POST ${base}/uploads`]: {
      status: 413,
      body: { error: 'too big', code: 'app_upload_size', params: { field: 'signed', max: 4 } },
    },
  })
  render(<Sign />)
  drawStroke(pad())
  fireEvent.click(screen.getByRole('button', { name: 'Save signature' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/Couldn’t save the signature/)
  expect((screen.getByRole('button', { name: 'Save signature' }) as HTMLButtonElement).disabled).toBe(false)
})

test('a signature can’t be saved in a preview, and a stored one is only shown', () => {
  stubCanvas()
  const { unmount } = render(<Sign field={{ base, uploads: false }} />)
  expect(screen.getByText(/can’t be saved in a preview/)).toBeTruthy()
  expect((screen.getByRole('button', { name: 'Save signature' }) as HTMLButtonElement).disabled).toBe(true)
  unmount()
  render(<SignatureField files={files} field="signed" value={id} onChange={() => {}} readOnly />)
  expect(screen.queryByRole('button')).toBeNull()
  expect(screen.getByRole('img', { name: 'The saved signature' }).getAttribute('src')).toBe(
    `${base}/attachments/${id}/thumbnail`,
  )
})

test('offline, a signature is staged on the device like a photo and nothing is uploaded', async () => {
  const { blobs } = stubCanvas()
  const calls = fakeFetch({})
  const staged: Blob[] = []
  const local = {
    stage: vi.fn((file: Blob) => {
      staged.push(file)
      return 'c'.repeat(32)
    }),
    url: vi.fn(() => 'blob:signature'),
    forget: vi.fn(),
  }
  const changed = vi.fn()
  render(<Sign field={{ base, uploads: true, local }} onValue={changed} />)
  expect(screen.getByText(/kept on this device with the record/)).toBeTruthy()
  drawStroke(pad())
  fireEvent.click(screen.getByRole('button', { name: 'Save signature' }))
  await waitFor(() => expect(changed).toHaveBeenCalledWith('c'.repeat(32)))
  expect(calls.length).toBe(0) // no connection is needed
  expect(blobs.length).toBeGreaterThan(0)
  expect(staged[0].type).toBe('image/png')
  // It is shown from the device, with no link to a server that doesn't have it yet.
  expect(screen.getByRole('img', { name: 'The saved signature' }).getAttribute('src')).toBe('blob:signature')
  expect(screen.queryByRole('link')).toBeNull()
  // Signing again lets the staged picture go.
  fireEvent.click(screen.getByRole('button', { name: 'Sign again' }))
  expect(local.forget).toHaveBeenCalledWith('c'.repeat(32))
  expect(changed).toHaveBeenLastCalledWith(undefined)
})

// ---- barcode --------------------------------------------------------------------------------------

function Code({ info, onValue }: { info?: { format?: string }; onValue?: (v: string) => void }) {
  const [value, setValue] = useState('')
  return (
    <BarcodeField
      label="Tag"
      value={value}
      info={info}
      onChange={(v) => {
        setValue(v)
        onValue?.(v)
      }}
    />
  )
}

const stream = () => ({ getTracks: () => [{ stop: vi.fn() }] }) as unknown as MediaStream

test('without a camera the code is typed, and the field says so', () => {
  const changed = vi.fn()
  render(<Code onValue={changed} />)
  expect(screen.queryByRole('button', { name: 'Scan with the camera' })).toBeNull()
  expect(screen.getByText(/can’t scan with the camera here/)).toBeTruthy()
  fireEvent.change(screen.getByLabelText(/Code \(type it/), { target: { value: '4006381333931' } })
  expect(changed).toHaveBeenLastCalledWith('4006381333931')
})

test('a blocked camera is told and typing still works', async () => {
  const getUserMedia = vi.fn().mockRejectedValue(new DOMException('no', 'NotAllowedError'))
  stubNavigator('mediaDevices', { getUserMedia })
  const changed = vi.fn()
  render(<Code onValue={changed} />)
  fireEvent.click(screen.getByRole('button', { name: 'Scan with the camera' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/camera is blocked for this site/)
  // Scanning stopped, the button is back, and the typed field is as it was.
  expect(screen.getByRole('button', { name: 'Scan with the camera' })).toBeTruthy()
  fireEvent.change(screen.getByLabelText(/Code \(type it/), { target: { value: 'ABC-1' } })
  expect(changed).toHaveBeenLastCalledWith('ABC-1')
})

test('a camera that isn’t there is told apart from one that is blocked', async () => {
  stubNavigator('mediaDevices', { getUserMedia: vi.fn().mockRejectedValue(new DOMException('none', 'NotFoundError')) })
  render(<Code />)
  fireEvent.click(screen.getByRole('button', { name: 'Scan with the camera' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/No camera was found/)
})

test('the browser’s own BarcodeDetector reads a code when it has one', async () => {
  const track = { stop: vi.fn() }
  stubNavigator('mediaDevices', { getUserMedia: vi.fn().mockResolvedValue({ getTracks: () => [track] }) })
  vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
  Object.defineProperty(HTMLMediaElement.prototype, 'readyState', { configurable: true, get: () => 4 })
  const detect = vi.fn().mockResolvedValue([{ rawValue: '4006381333931', format: 'ean_13' }])
  const options: unknown[] = []
  vi.stubGlobal(
    'BarcodeDetector',
    class {
      static getSupportedFormats = async () => ['ean_13', 'qr_code']
      constructor(o?: unknown) {
        options.push(o)
      }
      detect = detect
    },
  )
  const changed = vi.fn()
  render(<Code info={{ format: 'ean_13' }} onValue={changed} />)
  fireEvent.click(screen.getByRole('button', { name: 'Scan with the camera' }))
  await waitFor(() => expect(changed).toHaveBeenCalledWith('4006381333931'))
  expect(options).toEqual([{ formats: ['ean_13'] }])
  expect(await screen.findByText('Scanned: 4006381333931')).toBeTruthy()
  // The camera is let go as soon as a code is read.
  expect(track.stop).toHaveBeenCalled()
  expect(screen.getByText(/Reads EAN-13 codes/)).toBeTruthy()
})

test('a decoder that can’t start turns the camera off and leaves the typed field', async () => {
  const track = { stop: vi.fn() }
  stubNavigator('mediaDevices', { getUserMedia: vi.fn().mockResolvedValue({ getTracks: () => [track] }) })
  vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
  Object.defineProperty(HTMLMediaElement.prototype, 'readyState', { configurable: true, get: () => 4 })
  // The detector is there but can't run: every frame says so.
  const detect = vi.fn().mockRejectedValue(new DOMException('no decoder', 'NotSupportedError'))
  vi.stubGlobal(
    'BarcodeDetector',
    class {
      static getSupportedFormats = async () => ['qr_code']
      detect = detect
    },
  )
  const changed = vi.fn()
  render(<Code onValue={changed} />)
  fireEvent.click(screen.getByRole('button', { name: 'Scan with the camera' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/code reader isn’t working in this browser/)
  // Not scanning any more: the camera was let go (by the effect's cleanup, which can run just
  // after the message is drawn), the frames stopped coming, the button is back.
  await waitFor(() => expect(track.stop).toHaveBeenCalled())
  const frames = detect.mock.calls.length
  await new Promise((r) => setTimeout(r, 450))
  expect(detect.mock.calls.length).toBe(frames)
  expect(screen.getByRole('button', { name: 'Scan with the camera' }).getAttribute('aria-pressed')).toBe('false')
  fireEvent.change(screen.getByLabelText(/Code \(type it/), { target: { value: 'ABC-1' } })
  expect(changed).toHaveBeenLastCalledWith('ABC-1')
})

test('a decoder that fails every frame from the start is no decoder, but one frame’s miss is a miss', async () => {
  const video = { readyState: 4 } as HTMLVideoElement
  // Failing from the start, with whatever error: after a few frames it is given up.
  const broken = { detect: vi.fn().mockRejectedValue(new TypeError('Failed to fetch')) }
  const session = scan(video, broken, 1)
  await expect(session.result).rejects.toBeInstanceOf(DecoderError)
  expect(broken.detect.mock.calls.length).toBe(START_FAILURES)
  // One that worked and then missed a frame goes on, and finds the code.
  const flaky = {
    detect: vi
      .fn()
      .mockResolvedValueOnce([])
      .mockRejectedValueOnce(new Error('frame'))
      .mockRejectedValueOnce(new Error('frame'))
      .mockRejectedValueOnce(new Error('frame'))
      .mockRejectedValueOnce(new Error('frame'))
      .mockResolvedValue([{ rawValue: 'X-1' }]),
  }
  await expect(scan(video, flaky, 1).result).resolves.toMatchObject({ rawValue: 'X-1' })
})

vi.mock('./zxing', () => ({
  BarcodeDetector: class {
    async detect() {
      return [{ rawValue: 'FALLBACK-77', format: 'qr_code' }]
    }
  },
}))

test('where the browser has no BarcodeDetector the bundled decoder reads the code', async () => {
  stubNavigator('mediaDevices', { getUserMedia: vi.fn().mockResolvedValue(stream()) })
  vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue()
  Object.defineProperty(HTMLMediaElement.prototype, 'readyState', { configurable: true, get: () => 4 })
  expect('BarcodeDetector' in globalThis).toBe(false)
  const changed = vi.fn()
  render(<Code onValue={changed} />)
  fireEvent.click(screen.getByRole('button', { name: 'Scan with the camera' }))
  await waitFor(() => expect(changed).toHaveBeenCalledWith('FALLBACK-77'))
})

// ---- dictation ------------------------------------------------------------------------------------

const status = (over: Record<string, unknown> = {}) => ({ engine: 'local', max_seconds: 600, ...over })

test('dictation beside a field says it needs a connection, and is off when there is none', async () => {
  stubNavigator('onLine', false)
  fakeFetch({})
  render(<FieldDictation workspace={1} field="Notes" onText={() => {}} />)
  const mic = screen.getByRole('button', { name: 'Dictate into Notes' }) as HTMLButtonElement
  expect(mic.disabled).toBe(true)
  expect(screen.getByRole('status').textContent).toMatch(/needs a connection to this server/)
  expect(screen.getByRole('status').textContent).toMatch(/keyboard’s own dictation key/)
})

test('online, the microphone says in its title where the speech goes', async () => {
  stubNavigator('onLine', true)
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: status() } })
  render(<FieldDictation workspace={1} field="Notes" onText={() => {}} />)
  const mic = (await screen.findByRole('button', { name: 'Dictate into Notes' })) as HTMLButtonElement
  expect(mic.disabled).toBe(false)
  expect(mic.title).toMatch(/sent to this server.*needs a connection/)
})

test('the microphone is the same button online and offline: the library’s, never the chat box’s old one', async () => {
  stubNavigator('onLine', true)
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: status() } })
  const { unmount } = render(<FieldDictation workspace={1} field="Notes" onText={() => {}} />)
  const online = await screen.findByRole('button', { name: 'Dictate into Notes' })
  expect(online.className).toContain('ui-icon-button')
  // Every class it has is the library's: nothing of an old stylesheet's reaches it.
  expect(
    online.className
      .split(/\s+/)
      .filter(Boolean)
      .every((c) => c.startsWith('ui-')),
  ).toBe(true)
  unmount()
  stubNavigator('onLine', false)
  render(<FieldDictation workspace={1} field="Notes" onText={() => {}} />)
  const offline = screen.getByRole('button', { name: 'Dictate into Notes' })
  expect(offline.className).toBe(online.className)
})

test('where the workspace has dictation off, the reason and the keyboard’s own dictation are said', async () => {
  fakeFetch({
    'GET /api/workspaces/1/dictation': { body: status({ engine: '', reason: 'Dictation is off for this workspace.' }) },
  })
  render(<FieldDictation workspace={1} field="Notes" onText={() => {}} />)
  const mic = (await screen.findByRole('button', { name: 'Dictate into Notes' })) as HTMLButtonElement
  expect(mic.disabled).toBe(true)
  expect(screen.getByRole('status').textContent).toMatch(
    /Dictation is off for this workspace\. Your keyboard’s own dictation key still works/,
  )
})

test('a server without dictation shows nothing', async () => {
  const calls = fakeFetch({})
  const { container } = render(<FieldDictation workspace={1} field="Notes" onText={() => {}} />)
  await waitFor(() => expect(calls.length).toBe(1))
  expect(container.querySelector('button')).toBeNull()
})

test('dictated text goes in at the cursor, as typing, and the field is told', () => {
  const input = document.createElement('input')
  document.body.appendChild(input)
  input.value = 'Checked the'
  input.setSelectionRange(11, 11)
  const seen: string[] = []
  input.addEventListener('input', () => seen.push(input.value))
  insertText(input, 'left hinge')
  expect(input.value).toBe('Checked the left hinge')
  insertText(input, 'twice')
  expect(seen).toEqual(['Checked the left hinge', 'Checked the left hinge twice'])
  input.remove()
  expect(workspaceOf('/api/workspaces/7/spaces/2/apps/9')).toBe(7)
  expect(workspaceOf(undefined)).toBeNull()
})

// ---- on a screen ----------------------------------------------------------------------------------

const screenMessages = [
  { version: 'v0.9', createSurface: { surfaceId: 'app-9', catalogId: 'https://taskto.ai/catalogs/apps/v1.json' } },
  {
    version: 'v0.9',
    updateComponents: {
      surfaceId: 'app-9',
      components: [
        { id: 'root', component: 'Column', children: ['code', 'notes', 'place', 'sign', 'go'] },
        { id: 'code', component: 'BarcodeInput', field: 'code', label: 'Tag', value: { path: '/draft/code' } },
        { id: 'notes', component: 'TextField', label: 'Notes', variant: 'longText', value: { path: '/draft/notes' } },
        { id: 'place', component: 'LocationInput', field: 'place', label: 'Where', value: { path: '/draft/place' } },
        { id: 'sign', component: 'SignatureInput', field: 'signed', label: 'Signed', value: { path: '/draft/signed' } },
        { id: 'go_label', component: 'Text', text: 'Save' },
        {
          id: 'go',
          component: 'Button',
          child: 'go_label',
          action: {
            event: {
              name: 'add',
              context: {
                code: { path: '/draft/code' },
                notes: { path: '/draft/notes' },
                place: { path: '/draft/place' },
                signed: { path: '/draft/signed' },
              },
            },
          },
        },
      ],
    },
  },
  {
    version: 'v0.9',
    updateDataModel: {
      surfaceId: 'app-9',
      path: '/',
      value: {
        items: [],
        draft: {},
        _files: { signed: { max_files: 1, max_bytes: 1048576 } },
        _fields: {
          code: { format: '', required: false, editable: false },
          place: { capture: 'submit', required: true, editable: false },
          signed: { required: false, editable: false },
        },
      },
    },
  },
]

const NEEDS = [
  'component.BarcodeInput',
  'component.LocationInput',
  'component.SignatureInput',
  'field.barcode',
  'field.location',
  'field.signature',
]

test('on a screen the inputs draw, a typed code reaches the action and a submit-time location joins it', async () => {
  stubCanvas()
  stubNavigator('onLine', true)
  const geo = stubGeolocation({ latitude: 38.7223, longitude: -9.1393, accuracy: 9 })
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: status() } })
  const acted = vi.fn(async () => [])
  render(
    <AppView
      surfaceId="app-9"
      label="Inspection"
      release={{ definition_version: DEFINITION_VERSION, requires: NEEDS }}
      transport={{ load: async () => structuredClone(screenMessages), act: acted, files }}
    />,
  )
  expect(await screen.findByRole('group', { name: 'Tag' })).toBeTruthy()
  expect(screen.getByRole('group', { name: 'Where' })).toBeTruthy()
  expect(screen.getByRole('group', { name: 'Signed' })).toBeTruthy()
  expect(screen.getByText(/when you submit this form/)).toBeTruthy() // the field's own setting, from the data model
  fireEvent.change(screen.getByLabelText(/Code \(type it/), { target: { value: 'TAG-42' } })
  // The text fields on the screen get a microphone each, through the shared status.
  expect(await screen.findByRole('button', { name: 'Dictate into Notes' })).toBeTruthy()

  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(acted).toHaveBeenCalledTimes(1))
  expect(geo.getCurrentPosition).toHaveBeenCalledTimes(1)
  expect(geo.watchPosition).not.toHaveBeenCalled()
  const sent = (acted.mock.calls[0] as unknown as [{ name: string; context: Record<string, unknown> }])[0]
  expect(sent.name).toBe('add')
  expect(sent.context.code).toBe('TAG-42')
  expect(sent.context.place).toMatchObject({ lat: 38.7223, lon: -9.1393, accuracy_m: 9, source: 'device' })
})

test('pressing Save twice while the location is being taken sends one record', async () => {
  stubCanvas()
  stubNavigator('onLine', true)
  // A position that comes in when the test says: the second press is made while it is awaited.
  let give: PositionCallback = () => {}
  const geo = {
    getCurrentPosition: vi.fn((ok: PositionCallback) => {
      give = ok
    }),
    watchPosition: vi.fn(),
    clearWatch: vi.fn(),
  }
  stubNavigator('geolocation', geo)
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: status() } })
  const acted = vi.fn(async () => [])
  render(
    <AppView
      surfaceId="app-9"
      label="Inspection"
      release={{ definition_version: DEFINITION_VERSION, requires: NEEDS }}
      transport={{ load: async () => structuredClone(screenMessages), act: acted, files }}
    />,
  )
  const save = await screen.findByRole('button', { name: 'Save' })
  fireEvent.click(save)
  await waitFor(() => expect(geo.getCurrentPosition).toHaveBeenCalledTimes(1))
  // While it is pending the button is off, and a press that still gets through is the same submission.
  await waitFor(() => expect((save as HTMLButtonElement).disabled).toBe(true))
  expect(screen.getByRole('region', { name: 'Inspection' }).getAttribute('aria-busy')).toBe('true')
  fireEvent.click(save)
  act(() =>
    give({
      coords: { latitude: 38.7, longitude: -9.1, accuracy: 9 },
      timestamp: Date.now() - 500,
    } as GeolocationPosition),
  )
  await waitFor(() => expect(acted).toHaveBeenCalledTimes(1))
  await waitFor(() => expect((save as HTMLButtonElement).disabled).toBe(false))
  expect(geo.getCurrentPosition).toHaveBeenCalledTimes(1)
  expect(acted).toHaveBeenCalledTimes(1)
})

test('pressing Save again after an answer that didn’t arrive sends the same location, so the same operation', async () => {
  stubCanvas()
  stubNavigator('onLine', true)
  const geo = stubGeolocation({ latitude: 38.7, longitude: -9.1, accuracy: 9 })
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: status() } })
  // The first request's answer is lost (a gateway's, no code of the server's); the second is made.
  const acted = vi
    .fn()
    .mockRejectedValueOnce(new ApiError(502, 'bad gateway', {}))
    .mockResolvedValue([])
  render(
    <AppView
      surfaceId="app-9"
      label="Inspection"
      release={{ definition_version: DEFINITION_VERSION, requires: NEEDS }}
      transport={{ load: async () => structuredClone(screenMessages), act: acted, files }}
    />,
  )
  fireEvent.click(await screen.findByRole('button', { name: 'Save' }))
  await waitFor(() => expect(acted).toHaveBeenCalledTimes(1))
  await waitFor(() => expect((screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(false))
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(acted).toHaveBeenCalledTimes(2))
  const places = acted.mock.calls.map((c) => (c[0] as { context: { place: unknown } }).context.place)
  expect(places[1]).toEqual(places[0]) // not a second reading, which would be another submission
  expect(geo.getCurrentPosition).toHaveBeenCalledTimes(1)
})

test('a form that needs its location and can’t get it isn’t sent', async () => {
  stubCanvas()
  stubGeolocation({ code: 1 })
  fakeFetch({})
  const acted = vi.fn(async () => [])
  render(
    <AppView
      surfaceId="app-9"
      label="Inspection"
      release={{ definition_version: DEFINITION_VERSION, requires: NEEDS }}
      transport={{ load: async () => structuredClone(screenMessages), act: acted, files }}
    />,
  )
  fireEvent.click(await screen.findByRole('button', { name: 'Save' }))
  expect((await screen.findAllByRole('alert')).map((a) => a.textContent).join(' ')).toMatch(
    /This form needs a location/,
  )
  expect(acted).not.toHaveBeenCalled()
  // The typed way is open on the field.
  expect(screen.getByLabelText('Latitude')).toBeTruthy()
})

test('a client without the new fields is told to update, not shown a broken screen', async () => {
  fakeFetch({})
  render(
    <AppView
      surfaceId="app-9"
      label="Inspection"
      release={{ definition_version: DEFINITION_VERSION, requires: [...NEEDS, 'field.hologram'] }}
      transport={{ load: async () => structuredClone(screenMessages), act: async () => [], files }}
    />,
  )
  expect(await screen.findByText(/field\.hologram/)).toBeTruthy()
})

// ---- no watching ----------------------------------------------------------------------------------

test('nothing in the web app or the agent watches the device’s position', () => {
  // The word is built here so this file isn't its own finding.
  const forbidden = ['watch', 'Position'].join('')
  const hits: string[] = []
  const walk = (dir: string) => {
    for (const name of readdirSync(dir)) {
      if (name === 'node_modules' || name === 'dist') continue
      const path = join(dir, name)
      if (statSync(path).isDirectory()) walk(path)
      else if (/\.(tsx?|jsx?|mjs|html)$/.test(name) && !name.endsWith('capture.test.tsx')) {
        if (readFileSync(path, 'utf8').includes(forbidden)) hits.push(path)
      }
    }
  }
  walk(join(import.meta.dirname, '..'))
  walk(join(import.meta.dirname, '../../../agent/src'))
  expect(hits).toEqual([])
})

test('on a screen, the typed signature’s name field is named by its own label, not the confirming box’s', async () => {
  stubCanvas()
  stubNavigator('onLine', true)
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: status() } })
  render(
    <AppView
      surfaceId="app-9"
      label="Inspection"
      release={{ definition_version: DEFINITION_VERSION, requires: NEEDS }}
      transport={{ load: async () => structuredClone(screenMessages), act: vi.fn(async () => []), files }}
    />,
  )
  fireEvent.click(await screen.findByRole('radio', { name: 'Type your name' }))
  // The screen's label linking runs on what was just drawn.
  await new Promise((r) => setTimeout(r, 0))
  const name = screen.getByRole('textbox', { name: 'Your full name' })
  expect(name.getAttribute('aria-labelledby')).toBeNull()
  expect(screen.getByRole('checkbox', { name: /typing my name here is my signature/ })).toBeTruthy()
})

test('while a submission is being made, a photo’s buttons are held, and only the fields that can be part of it are not', async () => {
  stubCanvas()
  stubNavigator('onLine', true)
  const geo = { getCurrentPosition: vi.fn(), watchPosition: vi.fn(), clearWatch: vi.fn() }
  stubNavigator('geolocation', geo)
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: status() } })
  // The screen above, with a photo field holding one photo already.
  const messages = structuredClone(screenMessages) as typeof screenMessages
  const comps = (messages[1] as unknown as { updateComponents: { components: Record<string, unknown>[] } })
    .updateComponents.components
  ;(comps[0] as { children: string[] }).children.unshift('pics')
  comps.push({ id: 'pics', component: 'PhotoInput', field: 'pics', label: 'Pictures', value: { path: '/draft/pics' } })
  const model = (messages[2] as unknown as { updateDataModel: { value: Record<string, Record<string, unknown>> } })
    .updateDataModel.value
  model.draft.pics = ['0123456789abcdef0123456789abcdef']
  model._files.pics = { max_files: 3, max_bytes: 1048576 }
  render(
    <AppView
      surfaceId="app-9"
      label="Inspection"
      release={{
        definition_version: DEFINITION_VERSION,
        requires: [...NEEDS, 'component.PhotoInput', 'field.attachment'],
      }}
      transport={{ load: async () => messages, act: vi.fn(async () => []), files }}
    />,
  )
  const save = await screen.findByRole('button', { name: 'Save' })
  const remove = screen.getByRole('button', { name: 'Remove photo 1' }) as HTMLButtonElement
  const byHand = screen.getByRole('button', { name: 'Enter coordinates by hand' }) as HTMLButtonElement
  expect(remove.disabled).toBe(false)
  // The location is taken as the form is submitted, and the answer doesn't come: the submission waits.
  fireEvent.click(save)
  await waitFor(() => expect(geo.getCurrentPosition).toHaveBeenCalledTimes(1))
  await waitFor(() => expect((save as HTMLButtonElement).disabled).toBe(true))
  // Removing a photo now would delete an upload the request names: held.
  expect(remove.disabled).toBe(true)
  // The location, part of what is being sent, stays usable.
  expect(byHand.disabled).toBe(false)
})

test('a submit whose location comes after its person signed out and someone else signed in sends nothing', async () => {
  stubCanvas()
  stubNavigator('onLine', true)
  // A position that comes in when the test says.
  let give: PositionCallback = () => {}
  stubNavigator('geolocation', {
    getCurrentPosition: vi.fn((ok: PositionCallback) => {
      give = ok
    }),
    watchPosition: vi.fn(),
    clearWatch: vi.fn(),
  })
  fakeFetch({ 'GET /api/workspaces/1/dictation': { body: status() } })
  const acted = vi.fn(async () => [])
  beginSession(1)
  render(
    <AppView
      surfaceId="app-9"
      label="Inspection"
      release={{ definition_version: DEFINITION_VERSION, requires: NEEDS }}
      transport={{ load: async () => structuredClone(screenMessages), act: acted, files }}
    />,
  )
  expect(await screen.findByRole('group', { name: 'Where' })).toBeTruthy()
  fireEvent.change(screen.getByLabelText(/Code \(type it/), { target: { value: 'PRIVATE-1' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  // The first person signs out while the device looks for the location, and the next signs in.
  endSession()
  beginSession(2)
  await act(async () =>
    give({ coords: { latitude: 1, longitude: 2, accuracy: 5 }, timestamp: Date.now() } as GeolocationPosition),
  )
  await new Promise((r) => setTimeout(r, 20))
  expect(acted).not.toHaveBeenCalled()
})

test('a signature made into a picture after its session ended is not uploaded', async () => {
  stubCanvas()
  // The picture is made when the test says.
  let made: () => void = () => {}
  vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation((cb) => {
    made = () => cb(new Blob([new Uint8Array(8)], { type: 'image/png' }))
  })
  const calls = fakeFetch(uploadRoutes())
  beginSession(1)
  render(<Sign onValue={() => {}} />)
  fireEvent.click(screen.getByRole('radio', { name: 'Type your name' }))
  fireEvent.change(screen.getByLabelText('Your full name'), { target: { value: 'Ada Lovelace' } })
  fireEvent.click(screen.getByLabelText(/typing my name here is my signature/))
  fireEvent.click(screen.getByRole('button', { name: 'Save signature' }))
  endSession()
  await act(async () => made())
  await new Promise((r) => setTimeout(r, 20))
  expect(calls.some((c) => c.path.endsWith('/uploads'))).toBe(false)
})

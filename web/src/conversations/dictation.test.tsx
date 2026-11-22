// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { DictationStatus } from '../api'
import { parseRoute, usePath } from '../router'
import { type Call, fakeFetch, fakeStreams } from '../test/fakes'
import { ConversationsPage } from './ConversationsPage'
import { clock, DICTATION_RATE, Downsampler, encodeWav, type Recording, toBase64 } from './recorder'

// The microphone: a recording the test ends, holding a second of samples.
const recorded = vi.hoisted(() => ({ samples: new Int16Array(16000).fill(1000), cancelled: 0, started: 0 }))
vi.mock('./recorder', async (original) => {
  const real = await original<typeof import('./recorder')>()
  return {
    ...real,
    startRecording: async (): Promise<Recording> => {
      recorded.started++
      return {
        seconds: () => 1,
        stop: () => recorded.samples,
        cancel: () => {
          recorded.cancelled++
        },
      }
    },
  }
})

const status = (over: Partial<DictationStatus> = {}): DictationStatus => ({
  engine: 'local',
  settings: {
    engine: 'local',
    language: '',
    api: { base_url: 'https://api.openai.com/v1', model: 'whisper-1', api_key: 'not set' },
    accepted_notice: '',
  },
  local: { model: 'parakeet-v3', state: 'ready' },
  addons: [],
  credits: [],
  max_seconds: 600,
  ...over,
})

const conversation = {
  id: 7,
  workspace_id: 1,
  title: 'Hello',
  archived_at: null,
  created_at: '2026-11-18T10:00:00.000Z',
  updated_at: '2026-11-18T10:00:00.000Z',
}

function Page() {
  const route = parseRoute(usePath())
  return <ConversationsPage workspace={1} open={route?.item} streams={fakeStreams().factory} />
}

beforeEach(() => {
  window.history.replaceState(null, '', '/w/1/conversations/7')
  recorded.cancelled = 0
  recorded.started = 0
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

function routes(transcribe: (call: Call) => { status?: number; body?: unknown }, st = status()) {
  return fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation] } },
    'GET /api/workspaces/1/conversations/7': { body: conversation },
    'GET /api/workspaces/1/dictation': { body: st },
    'POST /api/workspaces/1/dictation/warm': { body: st },
    'POST /api/workspaces/1/dictation/transcriptions': transcribe,
  })
}

test('dictation puts the text where the cursor is, to edit before sending', async () => {
  const calls = routes(() => ({ body: { text: 'brave new', engine: 'local' } }))
  render(<Page />)
  const box = (await screen.findByLabelText('Message')) as HTMLTextAreaElement
  fireEvent.change(box, { target: { value: 'Hello world' } })
  box.setSelectionRange(6, 6) // after "Hello "

  fireEvent.click(await screen.findByRole('button', { name: 'Dictate' }))
  const stop = await screen.findByRole('button', { name: 'Stop dictation' })
  expect(calls.some((c) => c.method === 'POST' && c.path.endsWith('/dictation/warm'))).toBe(true)
  expect(screen.getByRole('timer').textContent).toContain('/ 10:00')
  fireEvent.click(stop)

  await waitFor(() => expect(box.value).toBe('Hello brave new world'))
  // Nothing was sent to the agent: the text waits in the box.
  expect(calls.some((c) => c.path.endsWith('/messages'))).toBe(false)
  const sent = calls.find((c) => c.path.endsWith('/transcriptions'))?.body as { audio_base64: string }
  const wav = Uint8Array.from(atob(sent.audio_base64), (ch) => ch.charCodeAt(0))
  expect(new TextDecoder().decode(wav.subarray(0, 4))).toBe('RIFF')
  expect(wav.length).toBe(44 + 2 * 16000)
})

test('a recording can be cancelled, and nothing is sent', async () => {
  const calls = routes(() => ({ body: { text: 'never', engine: 'local' } }))
  render(<Page />)
  fireEvent.click(await screen.findByRole('button', { name: 'Dictate' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }))
  await screen.findByRole('button', { name: 'Dictate' })
  expect(recorded.cancelled).toBe(1)
  expect(calls.some((c) => c.path.endsWith('/transcriptions'))).toBe(false)
})

test('a model still downloading says so, and the recording can be sent again', async () => {
  let ready = false
  routes(() =>
    ready
      ? { body: { text: 'second try', engine: 'local' } }
      : { status: 503, body: { error: 'the local speech model is downloading (40%)' } },
  )
  render(<Page />)
  const box = (await screen.findByLabelText('Message')) as HTMLTextAreaElement
  fireEvent.click(await screen.findByRole('button', { name: 'Dictate' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Stop dictation' }))
  expect((await screen.findByRole('alert')).textContent).toContain('downloading (40%)')
  ready = true
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await waitFor(() => expect(box.value).toBe('second try'))
  expect(recorded.started).toBe(1)
})

test('without an engine the microphone is off and says why', async () => {
  routes(() => ({ status: 503 }), status({ engine: '', reason: 'dictation isn’t set up', local: null }))
  render(<Page />)
  const mic = (await screen.findByRole('button', { name: 'Dictate' })) as HTMLButtonElement
  expect(mic.disabled).toBe(true)
  expect(mic.title).toBe('dictation isn’t set up')
})

test('a server without dictation shows no microphone', async () => {
  fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation] } },
    'GET /api/workspaces/1/conversations/7': { body: conversation },
  })
  render(<Page />)
  await screen.findByLabelText('Message')
  await act(async () => {})
  expect(screen.queryByRole('button', { name: 'Dictate' })).toBeNull()
})

test('the downsampler averages 48 kHz down to 16 kHz', () => {
  const d = new Downsampler(48000)
  const input = new Float32Array(4800)
  for (let i = 0; i < input.length; i++) input[i] = i % 3 === 0 ? 0.3 : 0 // mean 0.1 per 3
  d.push(input.subarray(0, 1000))
  d.push(input.subarray(1000))
  const out = d.result()
  expect(out.length).toBe(1600)
  expect(d.length).toBe(1600)
  for (const v of out) expect(Math.abs(v - Math.round(0.1 * 0x7fff))).toBeLessThanOrEqual(1)
  // 44.1 kHz: a second in, a second out.
  const e = new Downsampler(44100)
  e.push(new Float32Array(44100).fill(-2)) // clipped
  expect(Math.abs(e.length - DICTATION_RATE)).toBeLessThanOrEqual(1)
  expect(e.result()[0]).toBe(-32768)
})

test('the WAV is 16-bit PCM, mono, 16 kHz', async () => {
  const wav = encodeWav(new Int16Array([1, -1, 32767]))
  const v = new DataView(wav.buffer)
  expect(new TextDecoder().decode(wav.subarray(0, 4))).toBe('RIFF')
  expect(new TextDecoder().decode(wav.subarray(8, 16))).toBe('WAVEfmt ')
  expect([v.getUint16(20, true), v.getUint16(22, true), v.getUint32(24, true), v.getUint16(34, true)]).toEqual([
    1, 1, 16000, 16,
  ])
  expect(v.getUint32(40, true)).toBe(6)
  expect(v.getInt16(46, true)).toBe(-1)
  expect(atob(await toBase64(wav)).length).toBe(wav.length)
  expect(clock(75)).toBe('1:15')
  expect(clock(600)).toBe('10:00')
})

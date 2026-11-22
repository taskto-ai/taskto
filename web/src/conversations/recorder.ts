// Dictation's recording side (docs/dictation.md): the microphone, downmixed to mono and
// resampled to 16 kHz as it comes, kept as 16-bit samples (10 minutes is 19 MB), and sent as
// a WAV file, base64 in JSON as the API takes it.

import { t } from '../i18n'

export const DICTATION_RATE = 16000

// Resamples a stream of samples to 16 kHz: each output sample is the mean of the input
// samples it stands for, which also filters out what 16 kHz can't hold (well enough for
// speech). The input rate is the audio context's, usually 44.1 or 48 kHz.
export class Downsampler {
  private readonly ratio: number
  private pos = 0
  private sum = 0
  private count = 0
  private readonly chunks: Int16Array[] = []
  length = 0

  constructor(inputRate: number) {
    this.ratio = inputRate / DICTATION_RATE
  }

  push(input: Float32Array) {
    const out = new Int16Array(Math.ceil(input.length / this.ratio) + 1)
    let n = 0
    for (let i = 0; i < input.length; i++) {
      this.sum += input[i]
      this.count++
      this.pos += 1
      if (this.pos >= this.ratio) {
        this.pos -= this.ratio
        out[n++] = toInt16(this.sum / this.count)
        this.sum = 0
        this.count = 0
      }
    }
    this.chunks.push(out.subarray(0, n))
    this.length += n
  }

  // The samples so far, in one array.
  result(): Int16Array {
    const all = new Int16Array(this.length)
    let at = 0
    for (const c of this.chunks) {
      all.set(c, at)
      at += c.length
    }
    return all
  }
}

function toInt16(v: number): number {
  const s = Math.max(-1, Math.min(1, v))
  return Math.round(s < 0 ? s * 0x8000 : s * 0x7fff)
}

// A WAV file of 16-bit PCM, mono, at `rate`.
export function encodeWav(samples: Int16Array, rate = DICTATION_RATE): Uint8Array {
  const bytes = new Uint8Array(44 + samples.length * 2)
  const view = new DataView(bytes.buffer)
  const ascii = (at: number, s: string) => {
    for (let i = 0; i < s.length; i++) bytes[at + i] = s.charCodeAt(i)
  }
  ascii(0, 'RIFF')
  view.setUint32(4, 36 + samples.length * 2, true)
  ascii(8, 'WAVEfmt ')
  view.setUint32(16, 16, true)
  view.setUint16(20, 1, true) // PCM
  view.setUint16(22, 1, true) // mono
  view.setUint32(24, rate, true)
  view.setUint32(28, rate * 2, true)
  view.setUint16(32, 2, true)
  view.setUint16(34, 16, true)
  ascii(36, 'data')
  view.setUint32(40, samples.length * 2, true)
  for (let i = 0; i < samples.length; i++) view.setInt16(44 + i * 2, samples[i], true)
  return bytes
}

// Base64 by the browser's own encoder (a data: URL), without building a 19 MB binary string.
export function toBase64(bytes: Uint8Array): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => {
      const url = String(reader.result)
      resolve(url.slice(url.indexOf(',') + 1))
    }
    reader.onerror = () => reject(reader.error ?? new Error('could not encode the recording'))
    reader.readAsDataURL(new Blob([bytes as Uint8Array<ArrayBuffer>]))
  })
}

// A recording in progress.
export type Recording = {
  // Seconds recorded so far.
  seconds(): number
  // Ends the recording and returns its samples, at 16 kHz.
  stop(): Int16Array
  // Ends it and drops what was recorded.
  cancel(): void
}

export type StartRecording = () => Promise<Recording>

// Records the default microphone. The browser asks the person first; a refusal rejects.
export const startRecording: StartRecording = async () => {
  if (!navigator.mediaDevices?.getUserMedia) {
    throw new Error(t('conversations.dictation.cantRecord'))
  }
  const stream = await navigator.mediaDevices.getUserMedia({
    audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
  })
  let context: AudioContext
  let source: MediaStreamAudioSourceNode
  let node: ScriptProcessorNode
  let down: Downsampler
  try {
    context = new AudioContext()
    if (context.sampleRate < DICTATION_RATE) {
      void context.close()
      throw new Error(`the microphone records at ${context.sampleRate} Hz, under the 16 kHz dictation needs`)
    }
    source = context.createMediaStreamSource(stream)
    // A ScriptProcessorNode, not an AudioWorklet: it needs no module to load, and every
    // browser still has it. With one input channel, Web Audio downmixes a stereo microphone
    // to mono before it. It must reach the destination to run; it outputs silence.
    node = context.createScriptProcessor(4096, 1, 1)
    down = new Downsampler(context.sampleRate)
    node.onaudioprocess = (e) => down.push(e.inputBuffer.getChannelData(0))
    source.connect(node)
    node.connect(context.destination)
    // Made after the permission prompt, the context may start suspended: it must run.
    if (context.state !== 'running') await context.resume()
  } catch (e) {
    // The browser granted the microphone: let it go, so it doesn't stay on.
    for (const t of stream.getTracks()) t.stop()
    throw e
  }
  let ended = false
  const end = () => {
    if (ended) return
    ended = true
    node.onaudioprocess = null
    node.disconnect()
    source.disconnect()
    for (const t of stream.getTracks()) t.stop()
    void context.close()
  }
  return {
    seconds: () => down.length / DICTATION_RATE,
    stop: () => {
      end()
      return down.result()
    },
    cancel: end,
  }
}

// 75 → "1:15".
export function clock(seconds: number): string {
  const s = Math.floor(seconds)
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}

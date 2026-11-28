export interface ArchiveLimits {
  expandedBytes: number
  parts: number
}

export const ARCHIVE_LIMITS: ArchiveLimits = { expandedBytes: 100 * 1000 * 1000, parts: 2000 }

export class ArchiveTooLarge extends Error {
  constructor() {
    super('the archive is too large once expanded')
    this.name = 'ArchiveTooLarge'
  }
}

interface Stream {
  on(event: 'data', handler: (chunk: Uint8Array) => void): Stream
  on(event: 'error', handler: (error: Error) => void): Stream
  on(event: 'end', handler: () => void): Stream
  pause(): Stream
  resume(): Stream
}

interface Entry {
  dir: boolean
  internalStream(type: 'uint8array'): Stream
}

function expandedSize(entry: Entry, budget: number): Promise<number> {
  return new Promise((resolve, reject) => {
    let total = 0
    const stream = entry.internalStream('uint8array')
    stream
      .on('data', (chunk) => {
        total += chunk.length
        if (total > budget) {
          stream.pause()
          reject(new ArchiveTooLarge())
        }
      })
      .on('error', reject)
      .on('end', () => resolve(total))
      .resume()
  })
}

async function checkArchive(bytes: ArrayBuffer, limits: ArchiveLimits): Promise<void> {
  const { default: JSZip } = await import('jszip')
  const zip = await JSZip.loadAsync(bytes)
  const entries = Object.values(zip.files) as unknown as Entry[]
  if (entries.length > limits.parts) throw new ArchiveTooLarge()
  let left = limits.expandedBytes
  for (const entry of entries) {
    if (entry.dir) continue
    left -= await expandedSize(entry, left)
  }
}

export async function renderDocx(
  bytes: ArrayBuffer,
  into: HTMLElement,
  limits: ArchiveLimits = ARCHIVE_LIMITS,
): Promise<void> {
  await checkArchive(bytes, limits)
  const { renderAsync } = await import('docx-preview')
  await renderAsync(bytes, into, into, {
    inWrapper: true,
    ignoreLastRenderedPageBreak: true,
    experimental: false,
    useBase64URL: true,
    renderAltChunks: false,
  })
}

// @vitest-environment jsdom

import JSZip from 'jszip'
import { afterEach, expect, onTestFinished, test, vi } from 'vitest'
import { renderDocx } from './docx'

const CONTENT_TYPES = `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Default Extension="html" ContentType="text/html"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`
const ROOT_RELS = `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`

async function wordFile(extra: (zip: JSZip) => void, body: string): Promise<ArrayBuffer> {
  const zip = new JSZip()
  zip.file('[Content_Types].xml', CONTENT_TYPES)
  zip.file('_rels/.rels', ROOT_RELS)
  zip.file(
    'word/document.xml',
    `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><w:body>${body}</w:body></w:document>`,
  )
  extra(zip)
  return zip.generateAsync({ type: 'arraybuffer', compression: 'DEFLATE' })
}

afterEach(() => {
  vi.restoreAllMocks()
})

test('an archive that is small but expands past the budget is refused before the renderer sees it', async () => {
  const bomb = await wordFile((zip) => zip.file('word/media/zeros.bin', new Uint8Array(3_000_000)), '')
  expect(bomb.byteLength).toBeLessThan(20_000)
  const into = document.createElement('div')
  await expect(renderDocx(bomb, into, { expandedBytes: 1_000_000, parts: 100 })).rejects.toThrow(/too large/)
  expect(into.childElementCount).toBe(0)
})

test('the inflater stops soon after the budget is passed, within one block of the archive reader, not at the end of the part', async () => {
  const wholePart = 60_000_000
  const limit = 1_000_000
  const oneBlockInflated = 18_000_000
  const bomb = await wordFile((zip) => zip.file('word/media/zeros.bin', new Uint8Array(wholePart)), '')
  const sample = await JSZip.loadAsync(bomb)
  interface Counted {
    on(event: 'data', handler: (chunk: Uint8Array) => void): unknown
  }
  const prototype = Object.getPrototypeOf(sample.files['word/media/zeros.bin']) as {
    internalStream(type: string): Counted
  }
  const original = prototype.internalStream
  let produced = 0
  prototype.internalStream = function (this: unknown, type: string) {
    const stream = original.call(this, type)
    stream.on('data', (chunk) => {
      produced += chunk.length
    })
    return stream
  }
  onTestFinished(() => {
    prototype.internalStream = original
  })
  const into = document.createElement('div')
  await expect(renderDocx(bomb, into, { expandedBytes: limit, parts: 100 })).rejects.toThrow(/too large/)
  await new Promise((resolve) => setTimeout(resolve, 200))
  expect(produced).toBeGreaterThan(limit)
  expect(produced).toBeLessThan(limit + oneBlockInflated)
  expect(produced).toBeLessThan(wholePart)
})

test('parts that are each within the budget but together past it are refused', async () => {
  const bomb = await wordFile((zip) => {
    zip.file('word/media/a.bin', new Uint8Array(700_000))
    zip.file('word/media/b.bin', new Uint8Array(700_000))
  }, '')
  const into = document.createElement('div')
  await expect(renderDocx(bomb, into, { expandedBytes: 1_000_000, parts: 100 })).rejects.toThrow(/too large/)
  expect(into.childElementCount).toBe(0)
})

test('an archive of many parts is refused before the renderer sees it', async () => {
  const many = await wordFile((zip) => {
    for (let i = 0; i < 60; i++) zip.file(`word/media/${i}.bin`, 'x')
  }, '')
  const into = document.createElement('div')
  await expect(renderDocx(many, into, { expandedBytes: 1_000_000, parts: 50 })).rejects.toThrow(/too large/)
  expect(into.childElementCount).toBe(0)
})

test('a file that is not an archive is refused', async () => {
  const into = document.createElement('div')
  await expect(renderDocx(new Uint8Array([1, 2, 3]).buffer, into)).rejects.toThrow()
  expect(into.childElementCount).toBe(0)
})

test('an ordinary Word file within the budget is drawn', async () => {
  const file = await wordFile(() => {}, '<w:p><w:r><w:t>Master agreement</w:t></w:r></w:p>')
  const into = document.createElement('div')
  document.body.append(into)
  await renderDocx(file, into)
  expect(into.textContent).toContain('Master agreement')
  into.remove()
})

test('HTML embedded in the archive is not drawn: no frame is made for it', async () => {
  const file = await wordFile((zip) => {
    zip.file(
      'word/afchunk.html',
      '<html><body><p id="embedded">embedded</p><img src="https://example.invalid/x.png"></body></html>',
    )
    zip.file(
      'word/_rels/document.xml.rels',
      `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId9" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/aFChunk" Target="afchunk.html"/></Relationships>`,
    )
  }, '<w:p><w:r><w:t>Before</w:t></w:r></w:p><w:altChunk r:id="rId9"/>')
  const into = document.createElement('div')
  document.body.append(into)
  await renderDocx(file, into)
  expect(into.textContent).toContain('Before')
  expect(into.querySelector('iframe')).toBeNull()
  expect(into.innerHTML).not.toContain('embedded')
  into.remove()
})

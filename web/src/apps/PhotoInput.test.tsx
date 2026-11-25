// @vitest-environment jsdom
//
// Photos in an app: the input takes them from the camera or the library and uploads each in chunks
// that resume from the server's offset; the camera being blocked never stops the library; the
// thumbnails come from the server's own routes and nowhere else.

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { fakeFetch } from '../test/fakes'
import { AppView } from './AppView'
import { DEFINITION_VERSION } from './catalog'
import { type AppFiles, PhotoField, PhotoStrip } from './PhotoInput'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  Reflect.deleteProperty(navigator, 'permissions')
})

const base = '/api/workspaces/1/spaces/2/apps/9'
const files: AppFiles = { base, uploads: true }
const id = 'ab'.repeat(16)
const other = 'cd'.repeat(16)

const state = (over: Record<string, unknown> = {}) => ({
  id,
  field: 'photos',
  status: 'uploading',
  size: 10,
  received: 0,
  attached: false,
  chunk_size: 4,
  max_chunk: 8,
  ...over,
})

const picture = (name = 'cat.png', bytes = 10, type = 'image/png') => new File([new Uint8Array(bytes)], name, { type })

// A field that keeps its value, as the screen's data model does.
function Field({
  limits,
  onValue,
  start = [],
  field = files,
}: {
  limits?: { max_files: number; max_bytes: number }
  onValue?: (ids: string[]) => void
  start?: string[]
  field?: AppFiles | null
}) {
  const [value, setValue] = useState(start)
  return (
    <PhotoField
      label="Pictures"
      files={field}
      field="photos"
      value={value}
      limits={limits}
      onChange={(ids) => {
        setValue(ids)
        onValue?.(ids)
      }}
    />
  )
}

const library = () => screen.getByLabelText('Choose from library') as HTMLInputElement
const camera = () => screen.getByLabelText('Take a photo') as HTMLInputElement

function stubCamera(permission: 'denied' | 'granted' | 'prompt' | 'throws' | 'missing') {
  if (permission === 'missing') return
  Object.defineProperty(navigator, 'permissions', {
    configurable: true,
    value: {
      query: vi.fn(async () => {
        if (permission === 'throws') throw new TypeError('camera is not a permission')
        return { state: permission, onchange: null }
      }),
    },
  })
}

beforeEach(() => stubCamera('missing'))

test('the camera input asks for the rear camera and the library input does not', () => {
  fakeFetch({})
  render(<Field />)
  expect(camera().getAttribute('capture')).toBe('environment')
  expect(library().hasAttribute('capture')).toBe(false)
  for (const input of [camera(), library()]) expect(input.getAttribute('accept')).toBe('image/jpeg,image/png,image/gif')
  // It doesn't promise the photo is kept on the device.
  expect(screen.getByText(/may not also keep a copy in your camera roll/)).toBeTruthy()
  expect(screen.getByRole('group', { name: 'Pictures' })).toBeTruthy()
})

test('a blocked camera leaves the library working, and says so', async () => {
  stubCamera('denied')
  const calls = fakeFetch({
    [`POST ${base}/uploads`]: { status: 201, body: state() },
    [`PUT ${base}/uploads/${id}?offset=0`]: { body: state({ received: 4 }) },
    [`PUT ${base}/uploads/${id}?offset=4`]: { body: state({ received: 8 }) },
    [`PUT ${base}/uploads/${id}?offset=8`]: { body: state({ received: 10 }) },
    [`POST ${base}/uploads/${id}/finalize`]: {
      body: state({ status: 'ready', received: 10, content_type: 'image/png' }),
    },
  })
  const added = vi.fn()
  render(<Field onValue={added} />)
  await waitFor(() => expect(camera().disabled).toBe(true))
  expect(screen.getByText(/The camera is blocked for this site/)).toBeTruthy()
  expect(library().disabled).toBe(false)

  fireEvent.change(library(), { target: { files: [picture()] } })
  await waitFor(() => expect(added).toHaveBeenCalledWith([id]))
  // Three chunks in order, each at its offset, then finalize; the thumbnail is the server's route.
  expect(calls.filter((c) => c.method === 'PUT').map((c) => c.path)).toEqual([
    `${base}/uploads/${id}?offset=0`,
    `${base}/uploads/${id}?offset=4`,
    `${base}/uploads/${id}?offset=8`,
  ])
  const thumb = await screen.findByRole('img', { name: 'Photo 1 of 1' })
  expect(thumb.getAttribute('src')).toBe(`${base}/attachments/${id}/thumbnail`)
})

test('a camera whose permission the browser cannot tell is left enabled', async () => {
  for (const kind of ['throws', 'prompt', 'granted'] as const) {
    cleanup()
    stubCamera(kind)
    fakeFetch({})
    render(<Field />)
    await Promise.resolve()
    expect(camera().disabled).toBe(false)
  }
})

test('a photo taken with the camera goes the same way as one from the library', async () => {
  fakeFetch({
    [`POST ${base}/uploads`]: { status: 201, body: state({ size: 3 }) },
    [`PUT ${base}/uploads/${id}?offset=0`]: { body: state({ size: 3, received: 3 }) },
    [`POST ${base}/uploads/${id}/finalize`]: { body: state({ status: 'ready', size: 3, received: 3 }) },
  })
  const added = vi.fn()
  render(<Field onValue={added} />)
  fireEvent.change(camera(), { target: { files: [picture('IMG_1.jpg', 3, 'image/jpeg')] } })
  await waitFor(() => expect(added).toHaveBeenCalledWith([id]))
})

test('an upload that fails is resumed from the server, not started again', async () => {
  let puts = 0
  let creates = 0
  const calls = fakeFetch({
    [`POST ${base}/uploads`]: () => {
      creates++
      return { status: 201, body: state({ size: 8 }) }
    },
    // The connection fails at the second chunk, every time it is tried, until the person retries.
    [`PUT ${base}/uploads/${id}?offset=0`]: { body: state({ size: 8, received: 4 }) },
    [`PUT ${base}/uploads/${id}?offset=4`]: () => {
      puts++
      if (puts <= 4) return { status: 503, body: { error: 'busy', code: 'internal' } }
      return { body: state({ size: 8, received: 8 }) }
    },
    // Where the server is: it has the first chunk.
    [`GET ${base}/uploads/${id}`]: { body: state({ size: 8, received: 4 }) },
    [`POST ${base}/uploads/${id}/finalize`]: { body: state({ status: 'ready', size: 8, received: 8 }) },
  })
  const added = vi.fn()
  render(<Field onValue={added} />)
  fireEvent.change(library(), { target: { files: [picture('cat.png', 8)] } })

  const retry = await screen.findByRole('button', { name: 'Retry uploading cat.png' }, { timeout: 8000 })
  expect(screen.getByRole('alert').textContent).toContain('Couldn’t upload cat.png')
  expect(added).not.toHaveBeenCalled()
  fireEvent.click(retry)
  await waitFor(() => expect(added).toHaveBeenCalledWith([id]), { timeout: 8000 })
  // One upload was ever made, and the first chunk was sent once.
  expect(creates).toBe(1)
  expect(calls.filter((c) => c.method === 'PUT' && c.path.endsWith('offset=0'))).toHaveLength(1)
  expect(screen.queryByRole('alert')).toBeNull()
}, 20000)

test('an upload refused for want of storage is not retried, and says so', async () => {
  let puts = 0
  fakeFetch({
    [`POST ${base}/uploads`]: { status: 201, body: state({ size: 8 }) },
    [`PUT ${base}/uploads/${id}?offset=0`]: () => {
      puts++
      return { status: 507, body: { error: 'full', code: 'quota_user', params: { max_mib: 10 } } }
    },
  })
  const added = vi.fn()
  render(<Field onValue={added} />)
  fireEvent.change(library(), { target: { files: [picture('cat.png', 8)] } })
  const alert = await screen.findByRole('alert')
  expect(alert.textContent).toContain('Your private space has used its storage (10 MiB)')
  // Once: waiting and trying again only fails the same way.
  expect(puts).toBe(1)
  expect(added).not.toHaveBeenCalled()
}, 20000)

test('a chunk the server already has is not sent again: the client goes on from the server’s offset', async () => {
  const offsets: string[] = []
  fakeFetch({
    [`POST ${base}/uploads`]: { status: 201, body: state({ size: 8 }) },
    [`PUT ${base}/uploads/${id}?offset=0`]: (call) => {
      offsets.push(call.path)
      // The answer to this chunk was lost the first time; the server says where it is.
      return { status: 409, body: { error: 'x', code: 'app_upload_offset', params: { offset: 4 } } }
    },
    [`PUT ${base}/uploads/${id}?offset=4`]: (call) => {
      offsets.push(call.path)
      return { body: state({ size: 8, received: 8 }) }
    },
    [`POST ${base}/uploads/${id}/finalize`]: { body: state({ status: 'ready', size: 8, received: 8 }) },
  })
  const added = vi.fn()
  render(<Field onValue={added} />)
  fireEvent.change(library(), { target: { files: [picture('cat.png', 8)] } })
  await waitFor(() => expect(added).toHaveBeenCalledWith([id]))
  expect(offsets).toEqual([`${base}/uploads/${id}?offset=0`, `${base}/uploads/${id}?offset=4`])
})

test('a file that can never be taken is refused without a request', async () => {
  const calls = fakeFetch({})
  render(<Field limits={{ max_files: 2, max_bytes: 100 }} />)
  fireEvent.change(library(), { target: { files: [picture('big.png', 101)] } })
  expect((await screen.findByRole('alert')).textContent).toContain('big.png is too large: at most 100 B')
  fireEvent.change(library(), { target: { files: [picture('notes.pdf', 10, 'application/pdf')] } })
  await waitFor(() =>
    expect(screen.getByRole('alert').textContent).toContain('notes.pdf isn’t a JPEG, PNG or GIF picture'),
  )
  expect(calls).toHaveLength(0)
})

test('a field at its limit takes no more, and a photo can be taken out', async () => {
  const calls = fakeFetch({ [`DELETE ${base}/uploads/${id}`]: { status: 204 } })
  const changed = vi.fn()
  render(<Field limits={{ max_files: 1, max_bytes: 100 }} start={[other]} onValue={changed} />)
  expect(library().disabled).toBe(true)
  expect(camera().disabled).toBe(true)
  expect(screen.getAllByText(/At most 1 photo\./).length).toBeGreaterThan(0)
  fireEvent.click(screen.getByRole('button', { name: 'Remove photo 1' }))
  expect(changed).toHaveBeenCalledWith([])
  expect(library().disabled).toBe(false)
  // A photo of a record that was saved is left to the server's history: nothing is discarded.
  expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(0)
})

test('a preview, or a screen with nowhere to keep photos, uploads nothing', () => {
  fakeFetch({})
  render(<Field field={{ base, uploads: false }} />)
  expect(library().disabled).toBe(true)
  expect(screen.getByText(/Photos can’t be uploaded in a preview\./)).toBeTruthy()
  cleanup()
  render(<Field field={null} />)
  expect(library().disabled).toBe(true)
  expect(screen.getByText(/Photos can’t be added here\./)).toBeTruthy()
})

test('thumbnails are drawn from the server’s routes, open in a tab of their own, and name themselves', () => {
  render(<PhotoStrip base={base} ids={[id, other]} label="Receipts" />)
  const imgs = screen.getAllByRole('img')
  expect(imgs.map((i) => i.getAttribute('src'))).toEqual([
    `${base}/attachments/${id}/thumbnail`,
    `${base}/attachments/${other}/thumbnail`,
  ])
  expect(imgs.map((i) => i.getAttribute('alt'))).toEqual(['Photo 1 of 2', 'Photo 2 of 2'])
  const link = screen.getByRole('link', { name: 'Open photo 1 of 2 in a new tab' })
  expect(link.getAttribute('href')).toBe(`${base}/attachments/${id}`)
  expect(link.getAttribute('rel')).toContain('noopener')
  // Nothing in it points outside the server.
  for (const el of document.querySelectorAll('[src], [href]')) {
    expect((el.getAttribute('src') ?? el.getAttribute('href') ?? '').startsWith('/api/')).toBe(true)
  }
})

// The photo components as a screen uses them: through the renderer, in strict validation.
const surface = (items: unknown[]) => [
  { version: 'v0.9', createSurface: { surfaceId: 'app-9', catalogId: 'https://taskto.ai/catalogs/apps/v1.json' } },
  {
    version: 'v0.9',
    updateComponents: {
      surfaceId: 'app-9',
      components: [
        { id: 'root', component: 'Column', children: ['photo', 'list'] },
        { id: 'photo', component: 'PhotoInput', field: 'photos', label: 'Pictures', value: { path: '/draft/photos' } },
        { id: 'list', component: 'List', children: { componentId: 'item', path: '/items' } },
        { id: 'item', component: 'Photos', value: { path: 'photos' } },
      ],
    },
  },
  {
    version: 'v0.9',
    updateDataModel: {
      surfaceId: 'app-9',
      path: '/',
      value: { items, draft: {}, _files: { photos: { max_files: 2, max_bytes: 100 } } },
    },
  },
]

test('the renderer draws the photo input with the server’s limits and the thumbnails of rows', async () => {
  fakeFetch({})
  render(
    <AppView
      surfaceId="app-9"
      label="Photos"
      release={{
        definition_version: DEFINITION_VERSION,
        requires: ['component.PhotoInput', 'component.Photos', 'field.attachment'],
      }}
      transport={{
        load: async () =>
          surface([
            { id: 1, _revision: 1, photos: [id] },
            { id: 2, _revision: 1 },
          ]),
        act: async () => [],
        files,
      }}
    />,
  )
  expect(await screen.findByRole('group', { name: 'Pictures' })).toBeTruthy()
  // The limits came from the data model: two photos of at most 100 Bs.
  expect(screen.getAllByText(/At most 2 photos\./).length).toBeGreaterThan(0)
  expect(screen.getByText(/Each up to 100 B\./)).toBeTruthy()
  const img = await screen.findByRole('img', { name: 'Photo 1 of 1' })
  expect(img.getAttribute('src')).toBe(`${base}/attachments/${id}/thumbnail`)
})

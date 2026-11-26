// @vitest-environment jsdom

// A photo goes up in several trips (start, chunks, finalize). One begun in a session goes on only in
// it, and every trip names its account.

import { expect, test } from 'vitest'
import { beginSession, endSession } from '../session'
import { fakeFetch } from '../test/fakes'
import { UploadCancelled, uploadPhoto } from './photos'

const base = '/api/workspaces/1/spaces/2/apps/1'
const started = { id: 'u1', status: 'uploading', received: 0, chunk_size: 2, max_chunk: 2 }

test('every trip names the account the upload began for, even after another signed in', async () => {
  let start: () => void = () => {}
  const calls = fakeFetch({
    [`POST ${base}/uploads`]: () =>
      new Promise((done) => {
        start = () => done({ status: 201, body: started })
      }),
    [`PUT ${base}/uploads/u1?offset=0`]: { body: { ...started, received: 2 } },
  })
  beginSession(7)
  const going = uploadPhoto(base, 'receipt', new Blob([new Uint8Array([1, 2, 3])])).catch((e) => e)
  await expect.poll(() => calls.length).toBe(1)
  // The first person signs out, the next signs in, before the start is answered.
  endSession()
  beginSession(8)
  start()
  expect(await going).toBeInstanceOf(UploadCancelled)
  expect(calls.map((c) => [c.method, c.headers['x-taskto-expected-user']])).toEqual([['POST', '7']])
})

test('a resumed upload whose session ended while it asked where it was starts nothing new', async () => {
  let gone: () => void = () => {}
  const calls = fakeFetch({
    [`GET ${base}/uploads/old`]: () =>
      new Promise((done) => {
        gone = () => done({ status: 404, body: { error: 'not found' } })
      }),
    [`POST ${base}/uploads`]: { status: 201, body: started },
  })
  beginSession(7)
  const going = uploadPhoto(base, 'receipt', new Blob([new Uint8Array([1])]), { resume: 'old' }).catch((e) => e)
  await expect.poll(() => calls.length).toBe(1)
  endSession()
  beginSession(7)
  gone()
  expect(await going).toBeInstanceOf(UploadCancelled)
  expect(calls.map((c) => c.method)).toEqual(['GET'])
})

test('an upload whose session ended while it waited to try again asks nothing more', async () => {
  let failures = 0
  const calls = fakeFetch({
    [`POST ${base}/uploads`]: { status: 201, body: started },
    [`PUT ${base}/uploads/u1?offset=0`]: () => {
      failures++
      return { status: 503, body: { error: 'busy' } }
    },
    [`GET ${base}/uploads/u1`]: { body: started },
  })
  beginSession(7)
  const going = uploadPhoto(base, 'receipt', new Blob([new Uint8Array([1, 2, 3])]), { backoff: 300 }).catch((e) => e)
  await expect.poll(() => failures).toBe(1)
  // While it waits before trying again, the session ends.
  endSession()
  expect(await going).toBeInstanceOf(UploadCancelled)
  expect(calls.some((c) => c.method === 'GET')).toBe(false)
})

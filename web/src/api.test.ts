// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest'
import { ACCESS_LOST, ApiError, api, apiBytes, errorText, forAccount, SESSION_LOST } from './api'
import { setPreferences } from './i18n'
import { fakeFetch } from './test/fakes'

afterEach(async () => {
  vi.unstubAllGlobals()
  await setPreferences({ user: null })
})

test("an API error shows in the user's language by its code, with its params", async () => {
  const wrong = new ApiError(401, 'wrong email or password', {
    error: 'wrong email or password',
    code: 'wrong_credentials',
  })
  const name = new ApiError(400, 'a name of 1 to 100 characters is required', {
    error: 'a name of 1 to 100 characters is required',
    code: 'name_length',
    params: { max: 100 },
  })
  expect(errorText(wrong)).toBe('Wrong email or password.')
  expect(errorText(name)).toBe('A name needs 1 to 100 characters.')
  await setPreferences({ user: 'pt-BR' })
  expect(errorText(wrong)).toBe('E-mail ou senha incorretos.')
  expect(errorText(name)).toBe('Um nome precisa ter de 1 a 100 caracteres.')
})

test("an invalid desk layout says its reason in the user's language, and an unknown reason as it came", async () => {
  const refused = (reason: string) =>
    new ApiError(400, 'invalid desk layout', { error: 'invalid desk layout', code: 'desk_invalid', params: { reason } })
  expect(errorText(refused('too_many_tabs'))).toBe(
    'This desk layout can’t be saved: it has more than 20 tabs in a group or 60 in all.',
  )
  expect(errorText(refused('invalid_path'))).toBe(
    'This desk layout can’t be saved: a tab’s address isn’t a page of this workspace.',
  )
  expect(errorText(refused('from_a_newer_server'))).toBe('This desk layout can’t be saved: from_a_newer_server.')
  await setPreferences({ user: 'pt-BR' })
  expect(errorText(refused('too_large'))).toBe('Não foi possível salvar este layout da mesa: é maior que 32 KiB.')
})

test("a code the web app doesn't know shows the server's words; no words, its status", () => {
  const newer = new ApiError(409, 'something newer', { error: 'something newer', code: 'from_a_newer_server' })
  expect(errorText(newer)).toBe('something newer')
  expect(errorText(new ApiError(502, '502 Bad Gateway', null))).toBe('The server answered 502.')
  expect(errorText(new Error('boom'))).toBe('boom')
})

test('a refused request made for an account says whose session ended; one made for none, nobody’s', async () => {
  fakeFetch({ 'POST /api/x': { status: 401, body: { error: 'log in first', code: 'not_logged_in' } } })
  const heard: Array<{ user?: number } | undefined> = []
  const listen = (e: Event) => heard.push((e as CustomEvent<{ user?: number } | undefined>).detail)
  window.addEventListener(SESSION_LOST, listen)
  await expect(api('/api/x', { method: 'POST', body: {}, headers: forAccount(5) })).rejects.toBeInstanceOf(ApiError)
  await expect(api('/api/x', { method: 'POST', body: {} })).rejects.toBeInstanceOf(ApiError)
  window.removeEventListener(SESSION_LOST, listen)
  expect(heard).toEqual([{ user: 5 }, undefined])
})

test('only a 404 that says the space or the workspace is gone tells the open pages that access was lost', async () => {
  const heard: Event[] = []
  const listen = (e: Event) => heard.push(e)
  window.addEventListener(ACCESS_LOST, listen)
  try {
    const refuse = async (status: number, code: string) => {
      fakeFetch({ 'GET /api/x': { status, body: { error: 'no', code } } })
      await expect(api('/api/x')).rejects.toBeInstanceOf(ApiError)
    }
    await refuse(404, 'space_not_found')
    expect(heard).toHaveLength(1)
    await refuse(404, 'workspace_not_found')
    expect(heard).toHaveLength(2)
    await refuse(404, 'app_not_found')
    await refuse(403, 'space_not_found')
    await refuse(500, 'workspace_not_found')
    expect(heard).toHaveLength(2)
  } finally {
    window.removeEventListener(ACCESS_LOST, listen)
  }
})

test('a download of bytes returns them, and a refusal is an ApiError that ends the session like any other', async () => {
  fakeFetch({
    'GET /api/file': { body: { a: 1 } },
    'GET /api/gone': { status: 401, body: { error: 'log in first', code: 'not_logged_in' } },
  })
  expect(new TextDecoder().decode(await apiBytes('/api/file'))).toBe('{"a":1}')
  let ended = 0
  const listen = () => {
    ended++
  }
  window.addEventListener(SESSION_LOST, listen)
  await expect(apiBytes('/api/gone')).rejects.toMatchObject({ status: 401, code: 'not_logged_in' })
  window.removeEventListener(SESSION_LOST, listen)
  expect(ended).toBe(1)
})

test('a download that meets another account is reported like any other request', async () => {
  fakeFetch({ 'GET /api/file': { status: 409, body: { error: 'another account', code: 'session_account_changed' } } })
  let heard = 0
  const listen = () => {
    heard++
  }
  window.addEventListener('taskto:account-changed', listen)
  await expect(apiBytes('/api/file')).rejects.toMatchObject({ status: 409, code: 'session_account_changed' })
  window.removeEventListener('taskto:account-changed', listen)
  expect(heard).toBe(1)
})

test('a download whose answer is not JSON still fails with its status', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response('<html>', { status: 502, statusText: 'Bad Gateway' })),
  )
  await expect(apiBytes('/api/file')).rejects.toMatchObject({ status: 502, message: '502 Bad Gateway' })
})

test('a download that is aborted while its refusal is read ends with the abort, not an ApiError', async () => {
  const controller = new AbortController()
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => {
      const res = new Response('{}', { status: 500 })
      res.json = async () => {
        controller.abort()
        throw new DOMException('aborted', 'AbortError')
      }
      return res
    }),
  )
  await expect(apiBytes('/api/file', { signal: controller.signal })).rejects.toMatchObject({ name: 'AbortError' })
})

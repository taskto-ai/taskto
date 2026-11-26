// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest'
import { ApiError, api, errorText, forAccount, SESSION_LOST } from './api'
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

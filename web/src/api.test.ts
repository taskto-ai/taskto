// @vitest-environment jsdom

import { afterEach, expect, test } from 'vitest'
import { ApiError, errorText } from './api'
import { setPreferences } from './i18n'

afterEach(async () => {
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

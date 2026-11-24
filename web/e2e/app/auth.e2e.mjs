// First-run setup and logging in, in a real browser against the built server.

import assert from 'node:assert/strict'
import { owner, scenario, setUp } from '../support.mjs'

scenario('first-run setup creates the owner, and the owner can log out and back in', async (s) => {
  const { page } = s

  // A server with no account asks for its setup token, and a wrong one is refused.
  await page.goto('/')
  await page.getByRole('heading', { name: 'Set up this server' }).waitFor()
  await page.getByLabel('Setup token').fill('x'.repeat(43))
  await page.getByLabel('Your name').fill(owner.name)
  await page.getByLabel('Email').fill(owner.email)
  await page.getByLabel('Password', { exact: true }).fill(owner.password)
  await page.getByRole('button', { name: 'Create the owner' }).click()
  await page.getByRole('alert').waitFor()
  assert.equal(await page.getByRole('navigation', { name: 'Sections' }).count(), 0)

  // The right token signs the owner in, to their workspace.
  await setUp(s)
  await page.getByRole('link', { name: 'Knowledge base' }).waitFor()

  // The session survives a reload.
  await page.reload()
  await page.getByRole('navigation', { name: 'Sections' }).waitFor()

  // Logging out shows the login screen; a wrong password is refused and the right one gets in.
  await page.getByLabel(`Account: ${owner.name}`).click()
  await page.getByRole('button', { name: 'Log out' }).click()
  await page.getByRole('heading', { name: 'Log in' }).waitFor()
  await page.getByLabel('Email').fill(owner.email)
  await page.getByLabel('Password').fill('not the password at all')
  await page.getByRole('button', { name: 'Log in' }).click()
  await page.getByRole('alert').waitFor()
  assert.equal(await page.getByRole('navigation', { name: 'Sections' }).count(), 0)

  await page.getByLabel('Password').fill(owner.password)
  await page.getByRole('button', { name: 'Log in' }).click()
  await page.getByRole('navigation', { name: 'Sections' }).waitFor()
})

scenario('setup is closed once the owner exists', async ({ page, server, context }) => {
  await context.request.post('/api/setup', {
    headers: { 'Content-Type': 'application/json' },
    data: {
      email: owner.email,
      name: owner.name,
      password: owner.password,
      workspace_name: owner.workspace,
      setup_token: server.token,
    },
  })
  const again = await context.request.post('/api/setup', {
    headers: { 'Content-Type': 'application/json' },
    data: {
      email: 'second@example.com',
      name: 'Second',
      password: owner.password,
      workspace_name: 'Other',
      setup_token: server.token,
    },
  })
  assert.ok(!again.ok(), 'a second setup must be refused')
  await context.clearCookies()
  await page.goto('/')
  await page.getByRole('heading', { name: 'Log in' }).waitFor()
})

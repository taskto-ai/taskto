// The knowledge base: create a document, edit it, and see both changes in its history, and
// restore the first version.

import assert from 'node:assert/strict'
import { scenario, setUp } from '../support.mjs'

scenario('a document is created, edited, shown in its history and restored', async (s) => {
  const { page } = s
  await setUp(s)

  await page.getByRole('link', { name: 'Knowledge base' }).click()
  await page.getByText('No documents in this space yet.').waitFor()

  // A new document opens in the editor (the page's New; the empty space offers one too).
  await page.getByRole('button', { name: 'New', exact: true }).first().click()
  await page.getByLabel('Name', { exact: true }).fill('notes/plan.md')
  await page.getByRole('button', { name: 'Create', exact: true }).click()
  const text = page.getByLabel('Edit the document').getByLabel('Text')
  await text.fill('# Plan\n\nFirst version of the plan.')
  await page.getByLabel('What changed (optional)').fill('first draft')
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await page.getByText('First version of the plan.').waitFor()

  // The tree has it in its folder, and a reload shows the same text.
  const tree = page.getByRole('list', { name: 'Documents' })
  await tree.getByRole('button', { name: /^plan\.md/ }).waitFor()
  await page.reload()
  await tree.getByRole('button', { name: /^notes/ }).click()
  await tree.getByRole('button', { name: /^plan\.md/ }).click()
  await page.getByText('First version of the plan.').waitFor()

  // Edit it.
  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  await page.getByLabel('Edit the document').getByLabel('Text').fill('# Plan\n\nSecond version of the plan.')
  await page.getByLabel('What changed (optional)').fill('second draft')
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await page.getByText('Second version of the plan.').waitFor()

  // The history lists both changes, the newest first, by the owner.
  await page.getByRole('button', { name: 'History', exact: true }).click()
  const history = page.getByRole('region', { name: 'History' })
  await history.getByText('second draft').waitFor()
  await history.getByText('first draft').waitFor()
  // Creating the document is a change of its own, before the two saves.
  const versions = history.getByRole('list', { name: 'History' }).getByRole('listitem')
  assert.equal(await versions.count(), 3)
  assert.match(await versions.first().textContent(), /Owner/)

  // Restoring the first version (after its question) brings its text back, as a new change.
  await versions.filter({ hasText: 'first draft' }).getByRole('button', { name: 'Restore this version' }).click()
  await page
    .getByRole('alertdialog', { name: 'Restore this version?' })
    .getByRole('button', { name: 'Restore this version' })
    .click()
  await page.getByText('First version of the plan.').waitFor()
  await page.getByRole('button', { name: 'History', exact: true }).click()
  await page
    .getByRole('region', { name: 'History' })
    .getByRole('list', { name: 'History' })
    .getByRole('listitem')
    .nth(3)
    .waitFor()
})

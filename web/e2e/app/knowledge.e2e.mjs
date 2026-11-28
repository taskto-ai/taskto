import assert from 'node:assert/strict'
import { scenario, setUp } from '../support.mjs'

scenario('a document is created, edited, shown in its history and restored', async (s) => {
  const { page } = s
  await setUp(s)

  await page.getByRole('link', { name: 'Knowledge base' }).click()
  await page.getByText('No documents in this space yet.').waitFor()

  await page.getByRole('button', { name: 'New', exact: true }).first().click()
  await page.getByLabel('Name', { exact: true }).fill('notes/plan.md')
  await page.getByRole('button', { name: 'Create', exact: true }).click()
  const text = page.getByLabel('Edit the document').getByLabel('Text')
  await text.fill('# Plan\n\nFirst version of the plan.')
  await page.getByLabel('What changed (optional)').fill('first draft')
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await page.getByText('First version of the plan.').waitFor()

  const tree = page.getByRole('list', { name: 'Documents' })
  await tree.getByRole('button', { name: /^plan\.md/ }).waitFor()
  await page.reload()
  await tree.getByRole('button', { name: /^notes/ }).click()
  await tree.getByRole('button', { name: /^plan\.md/ }).click()
  await page.getByText('First version of the plan.').waitFor()

  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  await page.getByLabel('Edit the document').getByLabel('Text').fill('# Plan\n\nSecond version of the plan.')
  await page.getByLabel('What changed (optional)').fill('second draft')
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await page.getByText('Second version of the plan.').waitFor()

  await page.getByRole('button', { name: 'History', exact: true }).click()
  const history = page.getByRole('region', { name: 'History' })
  await history.getByText('second draft').waitFor()
  await history.getByText('first draft').waitFor()
  const versions = history.getByRole('list', { name: 'History' }).getByRole('listitem')
  assert.equal(await versions.count(), 3)
  assert.match(await versions.first().textContent(), /Owner/)

  await versions.filter({ hasText: 'second draft' }).getByRole('button', { name: 'Changes' }).click()
  const diff = history.getByRole('region', { name: /^Changes on/ })
  await diff.locator('ins', { hasText: 'Second' }).waitFor()
  await diff.locator('del', { hasText: 'First' }).waitFor()

  await versions.filter({ hasText: 'first draft' }).getByRole('button', { name: 'Compare with current' }).click()
  const compared = history.getByRole('region', { name: /compared with the current version$/ })
  await compared.locator('ins', { hasText: 'Second' }).waitFor()
  await compared.getByRole('button', { name: 'Restore this version' }).waitFor()

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

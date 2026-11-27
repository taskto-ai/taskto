import { scenario, setUp } from '../support.mjs'

scenario('a card is added to a board, moved between columns and commented on', async (s) => {
  const { page } = s
  await setUp(s)

  await page.getByRole('link', { name: 'Boards', exact: true }).click()
  await page.getByRole('button', { name: 'Add a card to To do' }).waitFor()

  await page.getByRole('button', { name: 'Add a card to To do' }).click()
  await page.getByLabel('Title').fill('Write the release notes')
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  const column = (name) => page.getByRole('region', { name })
  await column('To do')
    .getByRole('button', { name: /^Write the release notes/ })
    .waitFor()

  await column('To do')
    .getByRole('button', { name: /^Write the release notes/ })
    .click()
  const card = page.getByRole('region', { name: 'Write the release notes' })
  await card.getByRole('button', { name: 'Status: To do' }).click()
  await card.getByRole('menuitem', { name: 'Doing' }).click()
  await card.getByRole('button', { name: 'Status: Doing' }).waitFor()

  await page.getByLabel('Comment', { exact: true }).fill('Started on the draft.')
  await page.getByRole('button', { name: 'Comment', exact: true }).click()
  await page.getByText('Started on the draft.').waitFor()

  await page.getByRole('button', { name: 'Close', exact: true }).click()
  await column('Doing')
    .getByRole('button', { name: /^Write the release notes/ })
    .waitFor()
  await column('To do')
    .getByRole('button', { name: /^Write the release notes/ })
    .waitFor({ state: 'detached' })
  await page.reload()
  await column('Doing')
    .getByRole('button', { name: /^Write the release notes/ })
    .waitFor()

  await column('Doing')
    .getByRole('button', { name: /^Write the release notes/ })
    .click()
  await page.getByText('Started on the draft.').waitFor()

  await page.getByRole('button', { name: 'Close', exact: true }).click()
  await card.waitFor({ state: 'detached' })
  await column('Doing').getByRole('button', { name: 'Move “Write the release notes”' }).click()
  await page.getByRole('menuitem', { name: 'To Done' }).click()
  await column('Done')
    .getByRole('button', { name: /^Write the release notes/ })
    .waitFor()
})

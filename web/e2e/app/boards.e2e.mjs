// Boards: on the personal board every private space has, add a card, move it between columns and comment on it, and see it all
// again after a reload.

import { scenario, setUp } from '../support.mjs'

scenario('a card is added to a board, moved between columns and commented on', async (s) => {
  const { page } = s
  await setUp(s)

  await page.getByRole('link', { name: 'Boards', exact: true }).click()
  // The private space has its personal board from the start (new boards are for shared spaces).
  await page.getByRole('button', { name: 'Add a card to To do' }).waitFor()

  // A card starts in the column it was added to.
  await page.getByRole('button', { name: 'Add a card to To do' }).click()
  await page.getByLabel('Title').fill('Write the release notes')
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  const column = (name) => page.getByRole('region', { name })
  await column('To do')
    .getByRole('button', { name: /^Write the release notes/ })
    .waitFor()

  // Open the card and move it to Doing with its status.
  await column('To do')
    .getByRole('button', { name: /^Write the release notes/ })
    .click()
  const card = page.getByRole('region', { name: 'Write the release notes' })
  await card.getByRole('button', { name: 'Status: To do' }).click()
  await card.getByRole('menuitem', { name: 'Doing' }).click()
  await card.getByRole('button', { name: 'Status: Doing' }).waitFor()

  // A comment is kept with the card.
  await page.getByLabel('Comment', { exact: true }).fill('Started on the draft.')
  await page.getByRole('button', { name: 'Comment', exact: true }).click()
  await page.getByText('Started on the draft.').waitFor()

  // Back on the board, the card is in Doing and no longer in To do; a reload keeps it there.
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

  // The comment was stored with the card, not only shown: the reopened card has it after the reload.
  await column('Doing')
    .getByRole('button', { name: /^Write the release notes/ })
    .click()
  await page.getByText('Started on the draft.').waitFor()

  // And on to Done, with the card's Move menu on the board (the way without a mouse).
  await page.getByRole('button', { name: 'Close', exact: true }).click()
  await card.waitFor({ state: 'detached' })
  await column('Doing').getByRole('button', { name: 'Move “Write the release notes”' }).click()
  await page.getByRole('menuitem', { name: 'To Done' }).click()
  await column('Done')
    .getByRole('button', { name: /^Write the release notes/ })
    .waitFor()
})

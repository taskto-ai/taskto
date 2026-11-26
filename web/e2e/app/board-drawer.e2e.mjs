// An open card on a wide board, where only layout tells: the card lies over the columns, which keep
// the width they have with it closed; the head (Add a card, the filters) stays clear of it; the
// board's sideways scroll brings the last column out from under it; the open card's tile, and a
// tile that takes the keyboard, are beside it, never under it; closing gives the focus back.
import assert from 'node:assert/strict'
import { api, scenario, setUp } from '../support.mjs'

const COLUMNS = ['backlog', 'todo', 'doing', 'review', 'done']

for (const width of [1440, 1920]) {
  scenario(
    `an open card lies over a ${width}px board, which keeps its width and its reach`,
    async (s) => {
      const { page, context } = s
      await setUp(s)
      await page.setViewportSize({ width, height: 900 })
      const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
      const space = spaces.find((x) => x.kind === 'private')
      const base = `/api/workspaces/1/spaces/${space.id}/boards`
      const { boards } = await api(context, 'GET', base)
      const cards = `${base}/${boards[0].id}/cards`
      // A card in every column, the last column's the one to open.
      for (const column of COLUMNS) await api(context, 'POST', cards, { title: `In ${column}`, column })
      await page.goto('/w/1/boards')
      const tile = (name) => page.getByRole('button', { name: new RegExp(`^${name}`) })
      await tile('In done').waitFor()

      const columns = page.locator('.ui-board-columns')
      const widths = async () =>
        columns
          .locator('.ui-board-column')
          .evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().width)))
      const closed = await widths()

      // Open the last column's card: the drawer is over the board, which keeps its columns' widths.
      await tile('In done').click()
      const drawer = page.getByRole('region', { name: 'In done' })
      await drawer.waitFor()
      // Measured where it settles, after its entry (it slides in from the edge).
      await drawer.evaluate((el) =>
        Promise.all(el.getAnimations({ subtree: true }).map((a) => a.finished.catch(() => {}))),
      )
      const edge = (await drawer.boundingBox()).x
      assert.deepEqual(await widths(), closed, 'the columns keep the width they have with the card closed')

      // The head stays in reach beside the drawer.
      const add = page.getByRole('button', { name: 'Add a card', exact: true })
      const addBox = await add.boundingBox()
      assert.ok(
        addBox.x + addBox.width <= edge,
        `Add a card (${addBox.x + addBox.width}) is clear of the drawer (${edge})`,
      )

      // The open card's own tile is beside the drawer, not under it.
      const open = await tile('In done').boundingBox()
      assert.ok(
        open.x + open.width <= edge + 1,
        `the open tile (${open.x + open.width}) is beside the drawer (${edge})`,
      )

      // The board's sideways scroll brings the last column out from under the drawer.
      await columns.evaluate((el) => {
        el.scrollLeft = el.scrollWidth
      })
      const last = await columns.locator('.ui-board-column').last().boundingBox()
      assert.ok(last.x + last.width <= edge + 1, `the last column (${last.x + last.width}) clears the drawer (${edge})`)

      // A tile that takes the keyboard is scrolled beside the drawer, never left under it.
      await columns.evaluate((el) => {
        el.scrollLeft = 0
      })
      for (const name of ['In review', 'In done']) {
        await tile(name).focus()
        const box = await tile(name).boundingBox()
        assert.ok(
          box.x + box.width <= edge + 1,
          `${name} with the focus (${box.x + box.width}) is beside the drawer (${edge})`,
        )
      }

      // Escape on the card closes it, and the focus goes back to its tile.
      await drawer.getByRole('button', { name: 'Close', exact: true }).focus()
      await page.keyboard.press('Escape')
      await drawer.waitFor({ state: 'detached' })
      assert.equal(await tile('In done').evaluate((el) => el === document.activeElement), true)
      assert.deepEqual(await widths(), closed)
    },
    { viewport: { width, height: 900 } },
  )
}

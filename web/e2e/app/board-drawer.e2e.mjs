import assert from 'node:assert/strict'
import { api, scenario, setUp } from '../support.mjs'

const COLUMNS = ['backlog', 'todo', 'doing', 'review', 'done']

for (const width of [1100, 1440, 1920]) {
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

      await tile('In done').click()
      const drawer = page.getByRole('region', { name: 'In done' })
      await drawer.waitFor()
      await drawer.evaluate((el) =>
        Promise.all(el.getAnimations({ subtree: true }).map((a) => a.finished.catch(() => {}))),
      )
      const edge = (await drawer.boundingBox()).x
      assert.deepEqual(await widths(), closed, 'the columns keep the width they have with the card closed')

      const add = page.getByRole('button', { name: 'Add a card', exact: true })
      const addBox = await add.boundingBox()
      assert.ok(
        addBox.x + addBox.width <= edge,
        `Add a card (${addBox.x + addBox.width}) is clear of the drawer (${edge})`,
      )

      const open = await tile('In done').boundingBox()
      assert.ok(
        open.x + open.width <= edge + 1,
        `the open tile (${open.x + open.width}) is beside the drawer (${edge})`,
      )

      await columns.evaluate((el) => {
        el.scrollLeft = el.scrollWidth
      })
      const last = await columns.locator('.ui-board-column').last().boundingBox()
      assert.ok(last.x + last.width <= edge + 1, `the last column (${last.x + last.width}) clears the drawer (${edge})`)

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

      await columns.evaluate((el) => {
        el.scrollLeft = el.scrollWidth
      })
      await tile('In backlog').focus()
      const start = await columns.boundingBox()
      const first = await tile('In backlog').boundingBox()
      assert.ok(first.x >= start.x - 1, `In backlog with the focus (${first.x}) is whole after the start (${start.x})`)

      await drawer.getByRole('button', { name: 'Close', exact: true }).focus()
      await page.keyboard.press('Escape')
      await drawer.waitFor({ state: 'detached' })
      assert.equal(await tile('In done').evaluate((el) => el === document.activeElement), true)
      assert.deepEqual(await widths(), closed)

      await tile('In done').click()
      await drawer.waitFor()
      await drawer.getByLabel('Comment', { exact: true }).fill('Half a thought')
      await drawer.getByRole('button', { name: 'Close', exact: true }).click()
      const question = page.getByRole('alertdialog', { name: 'Discard what you haven’t saved?' })
      await question.waitFor()
      const q = await question.boundingBox()
      assert.ok(q.width >= 300 && q.width <= 600, `the question is ${q.width}px wide`)
      assert.ok(q.height <= 400, `the question is ${q.height}px tall`)
      for (const name of ['Cancel', 'Discard']) {
        const b = await question.getByRole('button', { name, exact: true }).boundingBox()
        assert.ok(
          b.x >= q.x && b.y >= q.y && b.x + b.width <= q.x + q.width + 1 && b.y + b.height <= q.y + q.height + 1,
          `${name} (${b.x},${b.y} ${b.width}x${b.height}) is inside the question (${q.x},${q.y} ${q.width}x${q.height})`,
        )
      }
      await question.getByRole('button', { name: 'Cancel', exact: true }).click({ timeout: 5_000 })
      await question.waitFor({ state: 'detached' })
      assert.equal(await drawer.getByLabel('Comment', { exact: true }).inputValue(), 'Half a thought')
      await drawer.getByRole('button', { name: 'Close', exact: true }).click()
      await question.getByRole('button', { name: 'Discard', exact: true }).click({ timeout: 5_000 })
      await drawer.waitFor({ state: 'detached' })

      await tile('In todo').click()
      await page.locator('.ui-board-drawer').waitFor()
      await columns.evaluate((el) => {
        el.scrollLeft = 0
      })
      const end = await columns.boundingBox()
      const from = await tile('In backlog').boundingBox()
      const cdp = await context.newCDPSession(page)
      const mouse = (type, x, y, buttons = 1) =>
        cdp.send('Input.dispatchMouseEvent', { type, x, y, button: 'left', buttons, clickCount: 1 })
      const [sx, sy] = [from.x + 40, from.y + from.height / 2]
      const ex = end.x + end.width - 6
      await mouse('mouseMoved', sx, sy, 0)
      await mouse('mousePressed', sx, sy)
      for (let i = 1; i <= 20; i++) await mouse('mouseMoved', sx + ((ex - sx) * i) / 20, sy)
      for (let i = 0; i < 30; i++) {
        await mouse('mouseMoved', ex + (i % 2), sy)
        await page.waitForTimeout(50)
      }
      const scrolled = await columns.evaluate((el) => el.scrollLeft)
      await mouse('mouseReleased', ex, sy, 0)
      assert.ok(scrolled > 0, `dragging to the edge beside the drawer scrolls the board (${scrolled})`)
    },
    { viewport: { width, height: 900 } },
  )
}

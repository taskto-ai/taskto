import assert from 'node:assert/strict'
import { api, scenario, setUp } from '../support.mjs'

const WIDTHS = [390, 700, 800, 1440]
const ROOT_FONTS = [16, 20]
const PHONE_CHAT = 768
const PHONE_BOARD = 640

async function withRootFont(page, px) {
  await page.evaluate((size) => document.documentElement.style.setProperty('font-size', `${size}px`), px)
}

for (const width of WIDTHS) {
  for (const root of ROOT_FONTS) {
    scenario(
      `an open conversation at ${width}px with a ${root}px root font keeps the shell's phone breakpoint`,
      async (s) => {
        const { page } = s
        await setUp(s)
        await page.setViewportSize({ width, height: 900 })
        await page.goto('/w/1/conversations/1')
        await page.locator('.ui-chat-layout[data-open="conversation"]').waitFor()
        await withRootFont(page, root)
        await page.waitForTimeout(200)
        const { listShown, columns } = await page.evaluate(() => {
          const layout = document.querySelector('.ui-chat-layout')
          const list = layout.querySelector('.ui-chat-list')
          return {
            listShown: getComputedStyle(list).display !== 'none',
            columns: getComputedStyle(layout).gridTemplateColumns.split(' ').length,
          }
        })
        const phone = width <= PHONE_CHAT
        assert.equal(listShown, !phone, `the list is ${listShown ? 'shown' : 'hidden'} at ${width}px`)
        assert.equal(columns, phone ? 1 : 2, `${columns} columns at ${width}px`)
      },
    )

    scenario(`the board at ${width}px with a ${root}px root font keeps its pixel threshold`, async (s) => {
      const { page, context } = s
      await setUp(s)
      await page.setViewportSize({ width, height: 900 })
      const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
      const space = spaces.find((x) => x.kind === 'private')
      const base = `/api/workspaces/1/spaces/${space.id}/boards`
      const { boards } = await api(context, 'GET', base)
      await api(context, 'POST', `${base}/${boards[0].id}/cards`, { title: 'A card', column: 'todo' })
      await page.goto('/w/1/boards')
      await page.getByRole('button', { name: /^A card/ }).waitFor()
      await withRootFont(page, root)
      await page.waitForTimeout(200)
      const { snap, widths } = await page.evaluate(() => {
        const columns = document.querySelector('.ui-board-columns')
        return {
          snap: getComputedStyle(columns).scrollSnapType,
          widths: [...columns.querySelectorAll('.ui-board-column')].map((c) =>
            Math.round(c.getBoundingClientRect().width),
          ),
        }
      })
      const phone = width <= PHONE_BOARD
      assert.equal(snap !== 'none', phone, `scroll snap "${snap}" at ${width}px`)
      const main = await page.getByRole('main').evaluate((el) => el.getBoundingClientRect().width)
      if (phone) assert.ok(widths[0] > main * 0.7, `a column ${widths[0]}px of ${main}px on a phone`)
      else assert.ok(widths[0] < main * 0.7, `a column ${widths[0]}px of ${main}px beside the others`)
    })
  }
}

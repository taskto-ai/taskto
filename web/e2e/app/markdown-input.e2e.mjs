import assert from 'node:assert/strict'
import { api, scenario, setUp } from '../support.mjs'

const C = (id, component, props = {}) => ({ id, component, ...props })
const text = (id, value) => C(id, 'Text', { text: value })

const definition = {
  screens: [
    {
      name: 'main',
      root: 'main-root',
      components: [
        C('main-root', 'Column', { children: ['rows', 'notes', 'save'] }),
        C('rows', 'Table', {
          items: { path: '/items' },
          columns: [{ field: 'notes', label: 'Notes' }],
          label: 'Saved notes',
          open: 'open_note',
        }),
        C('notes', 'MarkdownInput', { field: 'notes', label: 'Notes', value: { path: '/draft/notes' } }),
        C('save', 'Button', {
          variant: 'primary',
          child: 'save-label',
          action: { event: { name: 'add', context: { notes: { path: '/draft/notes' } } } },
        }),
        text('save-label', 'Save'),
      ],
    },
    {
      name: 'detail',
      record: true,
      root: 'detail-root',
      components: [
        C('detail-root', 'Column', { children: ['detail-notes', 'detail-save'] }),
        C('detail-notes', 'MarkdownInput', { field: 'notes', label: 'Notes', value: { path: '/record/notes' } }),
        C('detail-save', 'Button', {
          variant: 'primary',
          child: 'detail-save-label',
          action: {
            event: { name: 'update_note', context: { id: { path: '/record/id' }, notes: { path: '/record/notes' } } },
          },
        }),
        text('detail-save-label', 'Save changes'),
      ],
    },
  ],
  entry: 'main',
  collection: { fields: [{ name: 'notes', type: 'string', max_length: 500 }] },
  actions: {
    add: { kind: 'insert' },
    open_note: { kind: 'navigate', screen: 'detail' },
    update_note: { kind: 'update' },
  },
}

async function publish(context) {
  const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
  const space = spaces.find((x) => x.kind === 'private')
  const base = `/api/workspaces/1/spaces/${space.id}/apps`
  const app = await api(context, 'POST', base, { name: 'Notes', definition })
  await api(context, 'POST', `${base}/${app.id}/publish`, { version: app.version, acknowledge: [] })
  return { base: `${base}/${app.id}`, id: app.id, page: `/w/1/apps/${space.id}-${app.id}` }
}

const notes = async (context, app) =>
  (await api(context, 'GET', `${app.base}/records`)).records.map((r) => r.data.notes)

async function seed(context, app, value) {
  await api(context, 'POST', `${app.base}/actions`, {
    name: 'add',
    surfaceId: `app-${app.id}`,
    sourceComponentId: 'save',
    timestamp: new Date().toISOString(),
    context: { notes: value },
  })
}

scenario('formatting from the toolbar and the shortcuts is undone and redone natively', async (s) => {
  const { page, context } = s
  await setUp(s)
  const app = await publish(context)
  await page.goto(app.page)
  const box = page.getByRole('textbox', { name: 'Notes' })
  await box.click()
  await page.keyboard.type('alpha')
  assert.equal(await box.inputValue(), 'alpha')

  await page.keyboard.press('ControlOrMeta+a')
  await page.getByRole('button', { name: 'Bold' }).click()
  assert.equal(await box.inputValue(), '**alpha**')
  await box.focus()
  await page.keyboard.press('ControlOrMeta+z')
  assert.equal(await box.inputValue(), 'alpha', 'undo reverses the formatting')
  await page.keyboard.press('ControlOrMeta+Shift+z')
  assert.equal(await box.inputValue(), '**alpha**', 'redo brings it back')

  await page.keyboard.press('ControlOrMeta+a')
  await page.keyboard.press('ControlOrMeta+i')
  assert.equal(await box.inputValue(), '_**alpha**_')
  await page.keyboard.press('ControlOrMeta+z')
  assert.equal(await box.inputValue(), '**alpha**')
})

scenario('undo still works after a round trip through Preview', async (s) => {
  const { page, context } = s
  await setUp(s)
  const app = await publish(context)
  await page.goto(app.page)
  const box = page.getByRole('textbox', { name: 'Notes' })
  await box.click()
  await page.keyboard.type('alpha')
  await page.getByRole('tab', { name: 'Preview' }).click()
  assert.equal(await page.getByRole('tabpanel', { name: 'Preview' }).getByText('alpha').count(), 1)
  await page.getByRole('tab', { name: 'Write' }).click()
  await box.focus()
  await page.keyboard.press('ControlOrMeta+z')
  assert.notEqual(await box.inputValue(), 'alpha', 'undo takes back what was typed before Preview')
  await page.keyboard.press('ControlOrMeta+Shift+z')
  assert.equal(await box.inputValue(), 'alpha')

  await page.getByRole('button', { name: 'Save' }).click()
  await page.getByRole('button', { name: /alpha/ }).waitFor()
  assert.deepEqual(await notes(context, app), ['alpha'])
})

scenario('bold and italic leave marks the preview reads', async (s) => {
  const { page, context } = s
  await setUp(s)
  const app = await publish(context)
  await page.goto(app.page)
  const box = page.getByRole('textbox', { name: 'Notes' })
  const preview = page.getByRole('tabpanel', { name: 'Preview' })
  await box.fill('one\n\ntwo')
  await page.keyboard.press('ControlOrMeta+a')
  await page.getByRole('button', { name: 'Bold' }).click()
  assert.equal(await box.inputValue(), '**one**\n\n**two**')
  await page.getByRole('tab', { name: 'Preview' }).click()
  assert.deepEqual(await preview.locator('strong').allTextContents(), ['one', 'two'])
  assert.equal(await preview.locator('text=**').count(), 0)

  await page.getByRole('tab', { name: 'Write' }).click()
  await box.fill('ordinary')
  await box.evaluate((el) => el.setSelectionRange(2, 5))
  await page.keyboard.press('ControlOrMeta+i')
  assert.equal(await box.inputValue(), 'or*din*ary')
  await page.getByRole('tab', { name: 'Preview' }).click()
  assert.equal(await preview.locator('em').textContent(), 'din')
})

scenario('formatting a record saved with Windows line breaks changes only the chosen text', async (s) => {
  const { page, context } = s
  await setUp(s)
  const app = await publish(context)
  await seed(context, app, 'one\r\ntwo\r\nthree')
  await page.goto(app.page)
  await page.getByRole('button', { name: /one/ }).click()
  const box = page.getByRole('textbox', { name: 'Notes' })
  await page.waitForFunction(() => document.querySelector('textarea')?.value)
  assert.equal(await box.inputValue(), 'one\ntwo\nthree')
  await box.evaluate((el) => {
    el.focus()
    el.setSelectionRange(4, 7)
  })
  await page.getByRole('button', { name: 'Bold' }).click()
  assert.equal(await box.inputValue(), 'one\n**two**\nthree')
  await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'POST' && r.url().endsWith('/actions') && r.ok()),
    page.getByRole('button', { name: 'Save changes' }).click(),
  ])
  assert.deepEqual(await notes(context, app), ['one\r\n**two**\r\nthree'])
})

scenario('a dictation that finishes while Preview is open lands in the text', async (s) => {
  const { page, context } = s
  await page.addInitScript(() => {
    navigator.mediaDevices.getUserMedia = async () => {
      const audio = new AudioContext()
      await audio.resume()
      const tone = audio.createOscillator()
      const out = audio.createMediaStreamDestination()
      tone.connect(out)
      tone.start()
      return out.stream
    }
  })
  let heard
  const arrived = new Promise((resolve) => {
    heard = resolve
  })
  let release
  const held = new Promise((resolve) => {
    release = resolve
  })
  await page.route('**/api/workspaces/1/dictation', (route) =>
    route.fulfill({ json: { engine: 'local', max_seconds: 600 } }),
  )
  await page.route('**/api/workspaces/1/dictation/warm', (route) => route.fulfill({ json: {} }))
  await page.route('**/api/workspaces/1/dictation/transcriptions', async (route) => {
    heard()
    await held
    await route.fulfill({ json: { text: 'spoken phrase' } })
  })
  await setUp(s)
  const app = await publish(context)
  await page.goto(app.page)
  const box = page.getByRole('textbox', { name: 'Notes' })
  await box.fill('base')

  await page.getByRole('button', { name: 'Dictate into Notes' }).click()
  await page.getByRole('timer').waitFor()
  await page.waitForTimeout(1000)
  await page.getByRole('button', { name: 'Stop dictation' }).click()
  await arrived
  await page.getByRole('tab', { name: 'Preview' }).click()
  await page.getByRole('tabpanel', { name: 'Preview' }).waitFor()
  release()
  await page.getByRole('tabpanel', { name: 'Preview' }).getByText('base spoken phrase').waitFor()
  await page.getByRole('tab', { name: 'Write' }).click()
  assert.equal(await box.inputValue(), 'base spoken phrase')
  await page.getByRole('button', { name: 'Save' }).click()
  await page.getByRole('button', { name: /base spoken phrase/ }).waitFor()
  assert.deepEqual(await notes(context, app), ['base spoken phrase'])
})

scenario('an editor bound to a literal or to another field is refused when the app is saved', async (s) => {
  const { context } = s
  await setUp(s)
  const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
  const space = spaces.find((x) => x.kind === 'private')
  const base = `/api/workspaces/1/spaces/${space.id}/apps`
  for (const value of ['template', { path: '/draft/other' }, { path: '/record/other' }]) {
    const bad = structuredClone(definition)
    bad.screens[0].components.find((c) => c.id === 'notes').value = value
    const res = await context.request.fetch(base, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      data: JSON.stringify({ name: 'Bound wrong', definition: bad }),
    })
    assert.equal(res.status(), 400, `${JSON.stringify(value)}: ${await res.text()}`)
  }
})

scenario('a failed check stays visible in Preview, beside the action it blocks', async (s) => {
  const { page, context } = s
  await setUp(s)
  const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
  const space = spaces.find((x) => x.kind === 'private')
  const base = `/api/workspaces/1/spaces/${space.id}/apps`
  const checked = structuredClone(definition)
  checked.screens[0].components.find((c) => c.id === 'notes').checks = [
    {
      condition: { call: 'length', args: { value: { path: '/draft/notes' }, min: 5 } },
      message: 'Write at least five characters',
    },
  ]
  const app = await api(context, 'POST', base, { name: 'Checked', definition: checked })
  await api(context, 'POST', `${base}/${app.id}/publish`, { version: app.version, acknowledge: [] })
  await page.goto(`/w/1/apps/${space.id}-${app.id}`)
  await page.getByRole('textbox', { name: 'Notes' }).fill('ab')
  await page.getByText('Write at least five characters').waitFor()
  await page.getByRole('tab', { name: 'Preview' }).click()
  await page.getByRole('tabpanel', { name: 'Preview' }).getByText('Write at least five characters').waitFor()
  await page.getByRole('button', { name: 'Save' }).waitFor()
})

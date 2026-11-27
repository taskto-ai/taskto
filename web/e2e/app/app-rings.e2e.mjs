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
        C('main-root', 'Column', {
          children: ['site', 'hours', 'day', 'kind', 'tools', 'urgent', 'effort', 'tabs', 'actions'],
        }),
        C('site', 'TextField', { label: 'Site', value: { path: '/draft/site' } }),
        C('hours', 'TextField', { label: 'Hours', variant: 'number', value: { path: '/draft/hours' } }),
        C('day', 'DateTimeInput', { label: 'Day', enableDate: true, value: { path: '/draft/day' } }),
        C('kind', 'ChoicePicker', {
          label: 'Kind',
          variant: 'mutuallyExclusive',
          displayStyle: 'chips',
          options: [
            { label: 'Inspection', value: 'inspection' },
            { label: 'Repair', value: 'repair' },
          ],
          value: { path: '/draft/kind' },
        }),
        C('tools', 'ChoicePicker', {
          label: 'Tools',
          variant: 'multipleSelection',
          displayStyle: 'checkbox',
          filterable: true,
          options: [
            { label: 'Ladder', value: 'ladder' },
            { label: 'Drill', value: 'drill' },
          ],
          value: { path: '/draft/tools' },
        }),
        C('urgent', 'CheckBox', { label: 'Urgent', value: { path: '/draft/urgent' } }),
        C('effort', 'Slider', { label: 'Effort', min: 0, max: 10, value: { path: '/draft/effort' } }),
        C('tabs', 'Tabs', {
          tabs: [
            { title: 'Checklist', child: 'tab-one' },
            { title: 'History', child: 'tab-two' },
          ],
        }),
        text('tab-one', 'Check the meter.'),
        text('tab-two', 'No earlier visits.'),
        C('actions', 'Row', { children: ['clear', 'save'] }),
        C('clear', 'Button', { child: 'clear-label', action: { event: { name: 'stay' } } }),
        text('clear-label', 'Clear'),
        C('save', 'Button', {
          variant: 'primary',
          child: 'save-label',
          action: { event: { name: 'add', context: { site: { path: '/draft/site' } } } },
        }),
        text('save-label', 'Save'),
      ],
    },
  ],
  entry: 'main',
  collection: { fields: [{ name: 'site', type: 'string' }] },
  actions: { add: { kind: 'insert' }, stay: { kind: 'navigate', screen: 'main' } },
  contributions: { widgets: [{ id: 'form', title: 'Visit form', kind: 'screen', size: 'large', screen: 'main' }] },
}

async function ringsIn(page, scope) {
  await page.locator(`${scope} .ui-app input, ${scope} .ui-app button`).first().focus()
  await page.keyboard.press('Shift+Tab')
  await page.keyboard.press('Tab')
  const seen = []
  const cut = []
  for (let i = 0; i < 40; i++) {
    const result = await page.evaluate((scopeSelector) => {
      const el = document.activeElement
      const app = el?.closest('.ui-app')
      if (!el || !app?.closest(scopeSelector)) return null
      const style = getComputedStyle(el)
      const name = `${el.tagName.toLowerCase()}.${[...el.classList].join('.')}[${el.getAttribute('aria-label') ?? el.textContent?.trim().slice(0, 20) ?? ''}]`
      if (style.outlineStyle === 'none' || Number.parseFloat(style.outlineWidth) === 0)
        return {
          name,
          cut: [
            `no ring (focus ${el.matches(':focus')}, focus-visible ${el.matches(':focus-visible')}, ${style.outlineStyle} ${style.outlineWidth}, type ${el.getAttribute('type')})`,
          ],
        }
      const reach = Math.max(0, Number.parseFloat(style.outlineWidth) + Number.parseFloat(style.outlineOffset))
      const lift = el.classList.contains('a2ui-button-primary') ? 3 : 0
      const r = el.getBoundingClientRect()
      const need = {
        top: r.top - reach,
        left: r.left - reach,
        right: r.right + reach,
        bottom: r.bottom + Math.max(reach, lift),
      }
      const problems = []
      for (let box = el.parentElement; box && box !== document.body; box = box.parentElement) {
        const s = getComputedStyle(box)
        if (s.overflowX === 'visible' && s.overflowY === 'visible') continue
        const b = box.getBoundingClientRect()
        const top = b.top + box.clientTop - box.scrollTop
        const left = b.left + box.clientLeft - box.scrollLeft
        const inner = {
          top,
          left,
          right: left + Math.max(box.clientWidth, box.scrollWidth),
          bottom: top + Math.max(box.clientHeight, box.scrollHeight),
        }
        const tolerance = 0.5
        const sides = []
        if (need.top < inner.top - tolerance) sides.push('top')
        if (need.bottom > inner.bottom + tolerance) sides.push('bottom')
        if (need.left < inner.left - tolerance) sides.push('start')
        if (need.right > inner.right + tolerance) sides.push('end')
        if (sides.length > 0)
          problems.push(`${box.tagName.toLowerCase()}.${[...box.classList].join('.')} cuts ${sides.join(', ')}`)
        if (/auto|scroll/.test(s.overflowX + s.overflowY)) break
      }
      return { name, cut: problems }
    }, scope)
    if (!result) break
    seen.push(result.name)
    if (result.cut.length > 0) cut.push(`${result.name}: ${result.cut.join('; ')}`)
    await page.keyboard.press('Tab')
  }
  return { seen, cut }
}

for (const [label, viewport] of [
  ['wide', { width: 1440, height: 900 }],
  ['a phone', { width: 390, height: 844 }],
]) {
  scenario(`every control of an app's screen shows its ring whole, launched and on Home (${label})`, async (s) => {
    const { page, context } = s
    await setUp(s)
    await page.setViewportSize(viewport)
    const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
    const space = spaces.find((x) => x.kind === 'private')
    const base = `/api/workspaces/1/spaces/${space.id}/apps`
    const app = await api(context, 'POST', base, { name: 'Ring check', definition })
    await api(context, 'POST', `${base}/${app.id}/publish`, { version: app.version, acknowledge: [] })
    await api(context, 'POST', '/api/workspaces/1/home/items', {
      kind: 'widget',
      space_id: space.id,
      app_id: app.id,
      widget: 'form',
    })

    await page.goto(`/w/1/apps/${space.id}-${app.id}`)
    await page.getByRole('button', { name: 'Save' }).waitFor()
    const launched = await ringsIn(page, 'main')

    await page.goto('/w/1/home')
    await page.getByRole('button', { name: 'Save' }).waitFor()
    const home = await ringsIn(page, 'main')

    assert.ok(launched.seen.length >= 10, `too few controls reached: ${launched.seen.join(', ')}`)
    assert.ok(home.seen.length >= 10, `too few controls reached on Home: ${home.seen.join(', ')}`)
    assert.deepEqual({ launched: launched.cut, home: home.cut }, { launched: [], home: [] })
  })
}

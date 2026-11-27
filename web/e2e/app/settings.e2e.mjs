import assert from 'node:assert/strict'
import { scenario, setUp } from '../support.mjs'

for (const [label, viewport] of [
  ['wide', { width: 1440, height: 900 }],
  ['a phone', { width: 390, height: 844 }],
]) {
  scenario(`a refused save of an integration is seen in the dialog, and keeps what was typed (${label})`, async (s) => {
    const { page } = s
    await setUp(s)
    await page.setViewportSize(viewport)
    await page.goto('/w/1/settings/integrations')
    await page.getByRole('button', { name: 'Connect an account' }).click()
    const dialog = page.getByRole('dialog', { name: 'New integration' })
    const form = dialog.getByRole('form', { name: 'New integration' })
    await form.getByLabel('Name', { exact: true }).fill('shop')
    await form.getByLabel('Secret key', { exact: true }).fill('not-a-key')
    await form.getByLabel('Largest amount of one payment').fill('500')
    await form.getByRole('checkbox', { name: 'EUR' }).check()
    await dialog.getByRole('button', { name: 'Add' }).click()

    const alert = form.getByRole('alert')
    await alert.waitFor()
    assert.equal(await dialog.isVisible(), true)
    const box = await alert.boundingBox()
    const seen = await dialog.evaluate((d) => {
      const body = d.querySelector('.ui-dialog-body') ?? d
      const r = body.getBoundingClientRect()
      return { top: Math.max(r.top, 0), bottom: Math.min(r.bottom, window.innerHeight) }
    })
    assert.ok(box, 'the alert is drawn')
    assert.ok(
      box.y >= seen.top - 1 && box.y + box.height <= seen.bottom + 1,
      `the alert (${box.y}–${box.y + box.height}) is inside the dialog's visible part (${seen.top}–${seen.bottom})`,
    )
    assert.equal(await alert.evaluate((a) => a.closest('.settings-form-error') === document.activeElement), true)
    const key = form.getByLabel('Secret key', { exact: true })
    assert.equal(await key.inputValue(), 'not-a-key')
    assert.equal(await key.getAttribute('type'), 'password')
    assert.equal(await form.getByRole('checkbox', { name: 'EUR' }).isChecked(), true)
  })
}

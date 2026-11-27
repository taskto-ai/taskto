import assert from 'node:assert/strict'
import JSZip from 'jszip'
import { api, scenario, setUp } from '../support.mjs'

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

async function wordFile(heading, cell) {
  const zip = new JSZip()
  zip.file(
    '[Content_Types].xml',
    '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>',
  )
  zip.file(
    '_rels/.rels',
    '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>',
  )
  zip.file(
    'word/document.xml',
    `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:rPr><w:b/></w:rPr><w:t>${heading}</w:t></w:r></w:p><w:tbl><w:tr><w:tc><w:p><w:r><w:t>${cell}</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Signed</w:t></w:r></w:p></w:tc></w:tr></w:tbl><w:sectPr/></w:body></w:document>`,
  )
  return zip.generateAsync({ type: 'nodebuffer' })
}

scenario('a Word file is previewed in its sandboxed frame, and so is an older version of it', async (s) => {
  const { page, context, messages } = s
  await setUp(s)
  const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
  const space = spaces.find((x) => x.kind === 'private')
  const url = `/api/workspaces/1/spaces/${space.id}/docs/contract.docx`
  const put = async (path, bytes) => {
    const res = await context.request.fetch(path, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/octet-stream' },
      data: bytes,
    })
    assert.ok(res.ok(), `PUT ${path}: ${res.status()} ${await res.text()}`)
  }
  await put(url, await wordFile('Draft agreement', 'Acme'))
  const { version } = await api(context, 'GET', url)
  await put(`${url}?base_version=${version}`, await wordFile('Master agreement', 'Globex'))

  await page.getByRole('link', { name: 'Knowledge base' }).click()
  await page
    .getByRole('list', { name: 'Documents' })
    .getByRole('button', { name: /^contract\.docx/ })
    .click()

  const frame = page.getByTitle('Preview of contract.docx')
  await frame.waitFor()
  assert.equal(await frame.getAttribute('sandbox'), 'allow-same-origin')
  const inside = page.frameLocator('iframe[title="Preview of contract.docx"]')
  await inside.getByText('Master agreement').waitFor()
  await inside.locator('table').getByText('Globex').waitFor()
  assert.equal(await inside.locator('script').count(), 0)
  await page.getByRole('link', { name: 'Download' }).waitFor()

  await page.getByRole('button', { name: 'History', exact: true }).click()
  const history = page.getByRole('region', { name: 'History' })
  const versions = history.getByRole('list', { name: 'History' }).getByRole('listitem')
  await versions.nth(1).getByRole('button', { name: 'View' }).click()
  const older = history.locator('.kb-version').getByTitle('Preview of contract.docx')
  await older.waitFor()
  await history.locator('.kb-version').frameLocator('iframe').getByText('Draft agreement').waitFor()

  assert.deepEqual(
    messages.filter((m) => !m.includes('Blocked script execution in')),
    [],
  )
})

scenario('a Word preview stays inside the page at phone widths and in both writing directions', async (s) => {
  const { page, context } = s
  await setUp(s)
  const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
  const space = spaces.find((x) => x.kind === 'private')
  const url = `/api/workspaces/1/spaces/${space.id}/docs/contract.docx`
  const res = await context.request.fetch(url, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/octet-stream' },
    data: await wordFile('Master agreement', 'Globex'),
  })
  assert.ok(res.ok(), `PUT ${url}: ${res.status()}`)
  await page.getByRole('link', { name: 'Knowledge base' }).click()
  await page
    .getByRole('list', { name: 'Documents' })
    .getByRole('button', { name: /^contract\.docx/ })
    .click()
  const inside = page.frameLocator('iframe[title="Preview of contract.docx"]')
  await inside.getByText('Master agreement').waitFor()
  for (const dir of ['ltr', 'rtl']) {
    await page.evaluate((value) => document.documentElement.setAttribute('dir', value), dir)
    for (const width of [320, 390]) {
      await page.setViewportSize({ width, height: 800 })
      const box = await page.getByTitle('Preview of contract.docx').boundingBox()
      assert.ok(box, `the frame has no box at ${width} (${dir})`)
      assert.ok(
        box.x >= 0 && box.x + box.width <= width + 0.5,
        `the frame leaves the page at ${width} (${dir}): ${JSON.stringify(box)}`,
      )
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
      assert.ok(overflow <= 0, `the page scrolls sideways by ${overflow}px at ${width} (${dir})`)
      const download = page.getByRole('link', { name: 'Download' })
      await download.scrollIntoViewIfNeeded()
      const link = await download.boundingBox()
      assert.ok(
        link && link.x >= 0 && link.x + link.width <= width + 0.5,
        `Download is out of reach at ${width} (${dir})`,
      )
    }
  }
})

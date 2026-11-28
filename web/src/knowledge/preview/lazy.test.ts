import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from 'vitest'

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    if (statSync(path).isDirectory()) return sources(path)
    return /\.(ts|tsx)$/.test(name) && !/\.test\.tsx?$/.test(name) ? [path] : []
  })
}

test('the Word library is only ever asked for by a dynamic import, in one file', () => {
  const root = join(__dirname, '..', '..')
  const mentions = sources(root)
    .map((file) => ({ file, text: readFileSync(file, 'utf8') }))
    .filter(({ text }) => /docx-preview|jszip/.test(text))
  expect(mentions.map(({ file }) => file.slice(root.length + 1))).toEqual(['knowledge/preview/docx.ts'])
  const [{ text }] = mentions
  expect(text).toContain("await import('docx-preview')")
  expect(text).not.toMatch(/^import .*docx-preview/m)
  expect(text).not.toMatch(/^import .*jszip/m)
})

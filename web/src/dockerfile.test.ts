import { readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { expect, test } from 'vitest'

// The image's web build (the Dockerfile's js stage) type-checks the tests too, so every file of
// internal/ that web/src imports must be copied into that stage, or the Images workflow fails
// while CI, which builds from the whole checkout, stays green.
const repo = resolve(__dirname, '../..')

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    if (statSync(path).isDirectory()) return sources(path)
    return /\.(ts|tsx)$/.test(name) ? [path] : []
  })
}

test('the image copies every directory of internal/ the web app imports', () => {
  const imported = new Set<string>()
  for (const file of sources(join(repo, 'web/src'))) {
    for (const [, spec] of readFileSync(file, 'utf8').matchAll(/from '((?:\.\.\/)+internal\/[^']+)'/g)) {
      imported.add(dirname(relative(repo, resolve(dirname(file), spec))))
    }
  }
  const dockerfile = readFileSync(join(repo, 'Dockerfile'), 'utf8')
  const copied = [...dockerfile.matchAll(/^COPY (internal\/\S+?)\/? /gm)].map(([, dir]) => dir)
  const missing = [...imported].filter((dir) => !copied.some((c) => dir === c || dir.startsWith(`${c}/`)))
  expect(imported.size).toBeGreaterThan(0)
  expect(missing).toEqual([])
})

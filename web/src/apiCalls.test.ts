import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { expect, test } from 'vitest'

const root = join(__dirname)

const ALLOWED: Record<string, string> = {
  'api.ts': 'api() itself',
}

const SENDS = [/\bfetch\s*\(/, /\bXMLHttpRequest\b/, /\bsendBeacon\s*\(/, /\bWebSocket\s*\(/]

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    if (statSync(path).isDirectory()) return name === 'test' ? [] : sources(path)
    if (!/\.(ts|tsx)$/.test(name) || /\.test\.tsx?$/.test(name) || /\.gallery\.tsx$/.test(name)) return []
    return [path]
  })
}

test('no request is sent around api()', () => {
  const found: string[] = []
  for (const path of sources(root)) {
    const file = relative(root, path)
    const code = readFileSync(path, 'utf8')
      .replace(/\/\/.*$/gm, '')
      .replace(/\/\*[\s\S]*?\*\//g, '')
    for (const pattern of SENDS) {
      if (pattern.test(code) && !(file in ALLOWED)) found.push(`${file}: ${pattern}`)
    }
  }
  expect(found).toEqual([])
})

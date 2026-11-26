// Every request the web app sends to the server goes through api() (api.ts), which is where the
// session's rules are kept (session.ts): a request that changes something names the session's
// account, and a 401 ends only the session it was sent in. A request sent some other way would
// skip both, so none is: this lists every other way of sending one in the app's code and fails on
// any that isn't allowed below, with the reason.

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { expect, test } from 'vitest'

const root = join(__dirname)

// file → why it may send a request itself.
const ALLOWED: Record<string, string> = {
  'api.ts': 'api() itself',
}

// The ways a page sends a request without api(). EventSource (a read, GET only, that changes
// nothing) and the service worker's own fetches (web/pwa, outside src) are not among them.
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

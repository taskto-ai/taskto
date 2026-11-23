import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from 'vitest'
import { PACKAGES, thirdPartyNotices } from './notices'

const root = join(import.meta.dirname, '..')

// The decoder the build ships has its licences with it: the MIT notices of the packages that
// carry code in it, and the Apache-2.0 text of ZXing-C++, which its .wasm is made from.
test('the notices shipped with the decoder have each licence in full', () => {
  const text = thirdPartyNotices(root)
  for (const name of PACKAGES) {
    const { version } = JSON.parse(readFileSync(join(root, 'node_modules', name, 'package.json'), 'utf8')) as {
      version: string
    }
    expect(text).toContain(`${name} ${version} (MIT)`)
    expect(text).toContain(readFileSync(join(root, 'node_modules', name, 'LICENSE'), 'utf8').trim())
  }
  expect(text).toContain('ZXing-C++ (Apache-2.0)')
  expect(text).toContain('Apache License\nVersion 2.0, January 2004')
  expect(text).toContain('TERMS AND CONDITIONS FOR USE, REPRODUCTION, AND DISTRIBUTION')
  expect(text).toContain('Permission is hereby granted, free of charge')
})

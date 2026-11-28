import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from 'vitest'
import { FONT_PACKAGES, PACKAGES, thirdPartyNotices } from './notices'

const root = join(import.meta.dirname, '..')

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

test("the notices carry the fonts' licence and copyright line", () => {
  const text = thirdPartyNotices(root)
  for (const name of FONT_PACKAGES) {
    const { version } = JSON.parse(readFileSync(join(root, 'node_modules', name, 'package.json'), 'utf8')) as {
      version: string
    }
    expect(text).toContain(`${name} ${version} (OFL-1.1)`)
    expect(text).toContain(readFileSync(join(root, 'node_modules', name, 'LICENSE'), 'utf8').trim())
  }
  expect(text).toContain('Copyright 2016 The Inter Project Authors')
  expect(text).toContain('Copyright 2022 The Bricolage Grotesque Project Authors')
  expect(text).toContain('Copyright 2020 The JetBrains Mono Project Authors')
  expect(text).toContain('SIL OPEN FONT LICENSE Version 1.1')
})

test('the notices carry JSZip under the MIT licence it is used under, and name the Word renderer as Apache-2.0', () => {
  const text = thirdPartyNotices(root)
  const read = (name: string) =>
    JSON.parse(readFileSync(join(root, 'node_modules', name, 'package.json'), 'utf8')) as { version: string }
  expect(text).toContain(`jszip ${read('jszip').version} (MIT, chosen from MIT OR GPL-3.0-or-later)`)
  expect(text).toContain('Copyright (c) 2009-2016 Stuart Knightley, David Duponchel, Franz Buchinger, António Afonso')
  expect(text).toContain('The above copyright notice and this permission notice shall be included in')
  expect(text).toContain(`docx-preview ${read('docx-preview').version} (Apache-2.0)`)
  expect(text).toContain('Copyright (c) 2016-2023 Volodymyr Baydalka')
  expect(text).not.toContain('GPL version 3')
})

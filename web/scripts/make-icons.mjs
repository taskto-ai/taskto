#!/usr/bin/env node

import { mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const out = join(dirname(fileURLToPath(import.meta.url)), '..', 'public', 'icons')
mkdirSync(out, { recursive: true })

const BG = '#00a86b'
const INK = '#0b0d12'
const MARK = `<g transform="translate(61.6 61.6) scale(6.08)">
      <path d="M8 32h22" fill="none" stroke="${INK}" stroke-width="8" stroke-linecap="round"/>
      <path d="M24 22l10 10-10 10" fill="none" stroke="${INK}" stroke-width="8" stroke-linecap="round" stroke-linejoin="round"/>
      <circle cx="51" cy="32" r="9" fill="${INK}"/>
    </g>`

function svg({ rounded, scale }) {
  const rx = rounded ? 120 : 0
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="512" height="512">
  <rect width="512" height="512" rx="${rx}" fill="${BG}"/>
  <g transform="translate(256 256) scale(${scale}) translate(-256 -256)">
    ${MARK}
  </g>
</svg>`
}

const icons = [
  { file: 'icon-192.png', size: 192, rounded: true, scale: 1 },
  { file: 'icon-512.png', size: 512, rounded: true, scale: 1 },
  { file: 'icon-maskable-512.png', size: 512, rounded: false, scale: 0.8 },
  { file: 'apple-touch-icon.png', size: 180, rounded: false, scale: 0.9 },
]

const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || undefined })
try {
  for (const { file, size, rounded, scale } of icons) {
    const page = await browser.newPage({ viewport: { width: size, height: size } })
    const markup = svg({ rounded, scale }).replace('width="512" height="512"', `width="${size}" height="${size}"`)
    await page.setContent(`<body style="margin:0;background:transparent">${markup}</body>`)
    await page.screenshot({
      path: join(out, file),
      omitBackground: true,
      clip: { x: 0, y: 0, width: size, height: size },
    })
    await page.close()
  }
} finally {
  await browser.close()
}

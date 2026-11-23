#!/usr/bin/env node
// Draws the app's PNG icons (public/icons/) from the same tile as icon.svg, with Chromium
// through Playwright. Run it by hand when the mark changes: `node scripts/make-icons.mjs`;
// the PNGs are committed, so a build needs no browser. CHROMIUM_PATH names a Chromium to use
// when Playwright's own isn't installed.
//
// - icon-192.png, icon-512.png: the rounded mark, for the manifest's "any" purpose;
// - icon-maskable-512.png: full-bleed background with the mark inside the central 80%, the
//   safe zone a launcher may crop a mask from;
// - apple-touch-icon.png: 180×180, full-bleed and opaque (iOS rounds it itself).

import { mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const out = join(dirname(fileURLToPath(import.meta.url)), '..', 'public', 'icons')
mkdirSync(out, { recursive: true })

// The taskto.ai website's app tile (its favicon): an emerald ground and the logo's mark, the arrow
// and the dot both in ink, so the dot stays seen on the green. The mark is drawn on a 64-unit grid
// and placed at 0.76 of the tile, as on the website.
const BG = '#00a86b'
const INK = '#0b0d12'
const MARK = `<g transform="translate(61.6 61.6) scale(6.08)">
      <path d="M8 32h22" fill="none" stroke="${INK}" stroke-width="8" stroke-linecap="round"/>
      <path d="M24 22l10 10-10 10" fill="none" stroke="${INK}" stroke-width="8" stroke-linecap="round" stroke-linejoin="round"/>
      <circle cx="51" cy="32" r="9" fill="${INK}"/>
    </g>`

// scale shrinks the mark about the centre for the full-bleed ones.
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

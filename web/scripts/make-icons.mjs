#!/usr/bin/env node
// Draws the app's PNG icons (public/icons/) from the same mark as icon.svg, with Chromium
// through Playwright. Run it by hand when the mark changes: `node scripts/make-icons.mjs`;
// the PNGs are committed, so a build needs no browser.
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

const BG = '#24477f'
const CHECK = 'M136 268l86 86 160-188'

// The mark's check spans 136..382 of 512; scale it about the centre for the full-bleed ones.
function svg({ rounded, scale }) {
  const rx = rounded ? 112 : 0
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="512" height="512">
  <rect width="512" height="512" rx="${rx}" fill="${BG}"/>
  <g transform="translate(256 256) scale(${scale}) translate(-256 -256)">
    <path d="${CHECK}" fill="none" stroke="#fff" stroke-width="52" stroke-linecap="round" stroke-linejoin="round"/>
  </g>
</svg>`
}

const icons = [
  { file: 'icon-192.png', size: 192, rounded: true, scale: 1 },
  { file: 'icon-512.png', size: 512, rounded: true, scale: 1 },
  { file: 'icon-maskable-512.png', size: 512, rounded: false, scale: 0.8 },
  { file: 'apple-touch-icon.png', size: 180, rounded: false, scale: 0.9 },
]

const browser = await chromium.launch()
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

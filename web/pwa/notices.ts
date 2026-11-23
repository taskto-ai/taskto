// The licence notices of the code and fonts that are shipped inside the built app, written next to
// it as /assets/THIRD_PARTY_NOTICES.txt (docs/pwa.md, "Device capabilities in apps").
//
// The fonts are Inter, Bricolage Grotesque and JetBrains Mono (SIL Open Font License 1.1), as the
// Fontsource packages carry them (docs/design.md). The OFL asks that its notice and the copyright line go with copies of the
// font, which the packages' LICENSE files hold, so they are written here from the installed packages.
//
// What a user's browser downloads to scan a code is a decoder: zxing-wasm (MIT) is ZXing-C++
// (Apache-2.0) compiled to WebAssembly, behind the BarcodeDetector API by the barcode-detector
// package (MIT). All three licences ask that their notice goes with copies of the code, in source or
// binary form, so the text of each is written here from the installed packages themselves (their
// LICENSE files, and the versions they say they are) and the Apache-2.0 text kept in licenses/.

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import type { Plugin } from 'vite'

// The packages whose code is in the build and whose licence has a file of its own to ship.
export const PACKAGES = ['barcode-detector', 'zxing-wasm'] as const
// The packages whose font files are in the build.
export const FONT_PACKAGES = [
  '@fontsource-variable/inter',
  '@fontsource-variable/bricolage-grotesque',
  '@fontsource-variable/jetbrains-mono',
] as const

const rule = '='.repeat(78)

function packageNotice(root: string, name: string): string {
  const dir = join(root, 'node_modules', name)
  const { version, license } = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8')) as {
    version: string
    license: string
  }
  return `${rule}
${name} ${version} (${license}), https://www.npmjs.com/package/${name}
${rule}

${readFileSync(join(dir, 'LICENSE'), 'utf8').trim()}
`
}

export function thirdPartyNotices(root: string): string {
  const parts = [
    `Third-party code and fonts in TaskTo.AI's web app

The fonts the app is set in, Inter, Bricolage Grotesque and JetBrains Mono, are served from the
files beside this one (inter-*.woff2, bricolage-grotesque-*.woff2 and jetbrains-mono-*.woff2),
never from another site. They are used unmodified, under the SIL Open Font License 1.1,
reproduced below with their copyright lines.
`,
  ]
  for (const name of FONT_PACKAGES) parts.push(packageNotice(root, name))
  parts.push(`
The code reader that scans barcodes and QR codes with the camera, where the browser has none of its
own, is loaded from the files beside this one (zxing_reader-*.wasm and its loader). It is made of:
`)
  for (const name of PACKAGES) parts.push(packageNotice(root, name))
  parts.push(`${rule}
ZXing-C++ (Apache-2.0), https://github.com/zxing-cpp/zxing-cpp
${rule}

The decoder's .wasm file is ZXing-C++ compiled to WebAssembly by zxing-wasm. ZXing-C++ is
copyrighted by its authors and licensed under the Apache License, Version 2.0, reproduced below;
the project's own notices are in its source repository, named above. It is used unmodified.

${readFileSync(join(root, 'licenses/Apache-2.0.txt'), 'utf8').trim()}
`)
  return parts.join('\n')
}

// Writes the notices into the build, in assets/ with the fonts and the decoder's .wasm.
export function notices(root: string): Plugin {
  return {
    name: 'taskto-notices',
    apply: 'build',
    generateBundle() {
      this.emitFile({ type: 'asset', fileName: 'assets/THIRD_PARTY_NOTICES.txt', source: thirdPartyNotices(root) })
    },
  }
}

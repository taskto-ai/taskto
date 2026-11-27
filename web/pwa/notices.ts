import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import type { Plugin } from 'vite'

export const PACKAGES = ['barcode-detector', 'zxing-wasm'] as const
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
    `Third-party code and fonts in taskto.ai's web app

The fonts the app is set in, Inter, Bricolage Grotesque and JetBrains Mono, are served from the
files beside this one (inter-*.woff2, bricolage-grotesque-*.woff2 and jetbrains-mono-*.woff2),
never from another site. They are used unmodified, under the SIL Open Font License 1.1,
reproduced below with their copyright lines.
`,
  ]
  for (const name of FONT_PACKAGES) parts.push(packageNotice(root, name))
  parts.push(`
The code reader that scans barcodes and QR codes with the camera, where the browser has none of its
own, is loaded from the files beside this one (zxing_reader-*.wasm and its loader), and so is the
writer that draws the QR code for turning on two-factor login (zxing_writer-*.wasm and its loader).
They are made of:
`)
  for (const name of PACKAGES) parts.push(packageNotice(root, name))
  parts.push(`${rule}
ZXing-C++ (Apache-2.0), https://github.com/zxing-cpp/zxing-cpp
${rule}

The reader's and the writer's .wasm files are ZXing-C++ compiled to WebAssembly by zxing-wasm. ZXing-C++ is
copyrighted by its authors and licensed under the Apache License, Version 2.0, reproduced below;
the project's own notices are in its source repository, named above. It is used unmodified.

${readFileSync(join(root, 'licenses/Apache-2.0.txt'), 'utf8').trim()}
`)
  return parts.join('\n')
}

export function notices(root: string): Plugin {
  return {
    name: 'taskto-notices',
    apply: 'build',
    generateBundle() {
      this.emitFile({ type: 'asset', fileName: 'assets/THIRD_PARTY_NOTICES.txt', source: thirdPartyNotices(root) })
    },
  }
}

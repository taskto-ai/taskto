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

function jszipNotice(root: string): string {
  const dir = join(root, 'node_modules', 'jszip')
  const { version } = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8')) as { version: string }
  const licence = readFileSync(join(dir, 'LICENSE.markdown'), 'utf8')
  const mit = licence.slice(licence.indexOf('The MIT License'), licence.indexOf('GPL version 3')).trim()
  return `${rule}
jszip ${version} (MIT, chosen from MIT OR GPL-3.0-or-later), https://www.npmjs.com/package/jszip
${rule}

${mit}
`
}

function docxPreviewNotice(root: string): string {
  const dir = join(root, 'node_modules', 'docx-preview')
  const { version } = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8')) as { version: string }
  const copyright = /^\s*Copyright \(c\).*$/m.exec(readFileSync(join(dir, 'LICENSE'), 'utf8'))?.[0].trim() ?? ''
  return `${rule}
docx-preview ${version} (Apache-2.0), https://www.npmjs.com/package/docx-preview
${rule}

${copyright}

The Word preview's renderer is licensed under the Apache License, Version 2.0, reproduced above
with the notice of ZXing-C++. It is used unmodified, and it loads the first time a Word file is opened.
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
  parts.push(`
The Word preview in the knowledge base is drawn by docx-preview, which reads the file with JSZip. Both
are in the one file loaded the first time a Word file is opened (docx-preview-*.js).
`)
  parts.push(docxPreviewNotice(root))
  parts.push(jszipNotice(root))
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

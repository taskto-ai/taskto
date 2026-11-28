#!/usr/bin/env node

import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const fromVite = createRequire(createRequire(import.meta.url).resolve('vite/package.json'))
const { parseAst } = await import(fromVite.resolve('rolldown/parseAst'))

export const ELEMENTS = ['button', 'select', 'input', 'textarea']

export const ALLOWED = new Map([
  [
    'src/apps/PhotoInput.tsx',
    {
      input: 2,
      reason:
        'the browser\'s file picker (and the camera) opens only from an <input type="file">: it sits unseen over a label drawn as a button, for the keyboard and screen readers',
    },
  ],
  [
    'src/apps/IconDialog.tsx',
    {
      input: 2,
      reason:
        'the app icon picker: a grid of emoji that are native radio buttons in one group, so the arrow keys move between them as the browser does for any radio group, each drawn as a tile; no component of src/ui/ is a group of tiles',
    },
  ],
  [
    'src/markdown.tsx',
    {
      input: 1,
      reason:
        "a document's task item: a read-only checkbox inside the rendered text, whose words are the document's own; the Checkbox component is a form's control with a label of its own",
    },
  ],
  [
    'src/boards/BoardCard.tsx',
    {
      button: 1,
      reason:
        "a card on the board: the whole card (its title, badges, labels and who it is for, on several lines) is the button that opens it, which no button component's one-line shape holds",
    },
  ],
])

export function controls(source, lang = 'tsx') {
  const found = []
  const visit = (node) => {
    if (Array.isArray(node)) {
      for (const n of node) visit(n)
      return
    }
    if (!node || typeof node !== 'object') return
    if (node.type === 'JSXOpeningElement' && node.name?.type === 'JSXIdentifier' && ELEMENTS.includes(node.name.name)) {
      found.push({ element: node.name.name, line: source.slice(0, node.start).split('\n').length })
    }
    for (const key of Object.keys(node)) if (key !== 'parent') visit(node[key])
  }
  visit(parseAst(source, { lang }))
  return found.sort((x, y) => x.line - y.line)
}

function modules(dir, out = []) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) modules(path, out)
    else if (/\.tsx?$/.test(entry.name) && !/\.(test|gallery)\.tsx?$/.test(entry.name) && !entry.name.endsWith('.d.ts'))
      out.push(path)
  }
  return out
}

export function check(web, allowed = ALLOWED) {
  const errors = []
  const src = join(web, 'src')
  const seen = new Set()
  for (const path of modules(src)) {
    const file = relative(web, path).split('\\').join('/')
    if (file.startsWith('src/ui/')) continue
    let found
    try {
      found = controls(readFileSync(path, 'utf8'), path.endsWith('.tsx') ? 'tsx' : 'ts')
    } catch (e) {
      errors.push(
        `${file}: doesn't parse, so its controls can't be counted: ${e instanceof Error ? e.message.split('\n')[0] : e}`,
      )
      continue
    }
    const allow = allowed.get(file)
    if (allow) seen.add(file)
    for (const element of ELEMENTS) {
      const these = found.filter((c) => c.element === element)
      const may = allow?.[element] ?? 0
      if (these.length > may) {
        for (const c of these.slice(may)) {
          errors.push(
            `${file}:${c.line}: a <${element}> written by hand: use the component (Button, IconButton, Input, Select, Textarea, Checkbox, Switch …) from src/ui/, or, if none can do it, add the file to ALLOWED with the reason`,
          )
        }
      } else if (these.length < may) {
        errors.push(
          `${file}: ALLOWED says ${may} <${element}>, the file has ${these.length}: lower the count (or remove the entry)`,
        )
      }
    }
  }
  if (existsSync(join(web, 'src/main.tsx'))) {
    for (const file of allowed.keys()) {
      if (!seen.has(file)) errors.push(`${file} is in ALLOWED but doesn't exist: remove it`)
    }
  }
  return errors
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const at = process.argv.indexOf('--root')
  const web = at > 0 ? process.argv[at + 1] : join(dirname(fileURLToPath(import.meta.url)), '..')
  const errors = check(web)
  if (errors.length > 0) {
    console.error(`controls-check: ${errors.length} problem${errors.length === 1 ? '' : 's'}\n`)
    for (const e of errors) console.error(`  ${e}`)
    process.exit(1)
  }
  console.log(
    'controls-check: outside src/ui/, no <button>, <select>, <input> or <textarea> is written by hand but the ones ALLOWED names',
  )
}

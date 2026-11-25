#!/usr/bin/env node
// Checks that the screens draw their controls with the design system's components: a `<button>`,
// `<select>`, `<input>` or `<textarea>` written by hand outside src/ui/ fails, unless ALLOWED names
// its file with the number of each such element it may have and the reason. The components
// (src/ui/) are where those elements are written; tests and the gallery's specimens are not
// screens. CI runs it with css-check (`pnpm --filter @taskto/web run css`); so can anyone.
//
// What it sees: a JSX element of those names in a .tsx or .ts file under src/, its comments left
// out (`{/* <button> */}` is not one). An element made another way (React.createElement, a string
// of HTML) is not seen; the app makes none.
//
// --root <dir> checks another web directory, for the script's tests.

import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { blankComments } from './css-check.mjs'

export const ELEMENTS = ['button', 'select', 'input', 'textarea']

// The hand-written controls a screen may keep: per file, how many of each element, and why no
// component fits. A count, not a yes, so a new one in the same file still fails; an entry the file
// no longer needs fails too, so the list stays true.
const OUTSIDE_SHELL =
  'a screen outside the shell (sign-in, the offline guards, the update banner), not yet rebuilt on the components: the rework of those screens takes these out, and this entry with them'
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
  ['src/App.tsx', { button: 1, reason: OUTSIDE_SHELL }],
  ['src/auth/AuthScreens.tsx', { button: 15, input: 15, reason: OUTSIDE_SHELL }],
  ['src/pwa/UpdateBanner.tsx', { button: 2, reason: OUTSIDE_SHELL }],
  ['src/offline/AccountChanged.tsx', { button: 1, reason: OUTSIDE_SHELL }],
  ['src/offline/ForeignGuard.tsx', { button: 1, reason: OUTSIDE_SHELL }],
  ['src/offline/LogoutGuard.tsx', { button: 3, reason: OUTSIDE_SHELL }],
  ['src/offline/ConfirmButton.tsx', { button: 3, reason: OUTSIDE_SHELL }],
])

const ELEMENT = new RegExp(`<(${ELEMENTS.join('|')})(?=[\\s/>])`, 'g')

// The hand-written controls in a module's source: each element's name and line.
export function controls(source) {
  const code = blankComments(source)
  return [...code.matchAll(ELEMENT)].map((m) => ({
    element: m[1],
    line: code.slice(0, m.index).split('\n').length,
  }))
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
    const found = controls(readFileSync(path, 'utf8'))
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
  // In the app itself (not a test's directory), an entry for a file that is gone is stale.
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

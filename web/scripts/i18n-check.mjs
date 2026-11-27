#!/usr/bin/env node

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const web = join(dirname(fileURLToPath(import.meta.url)), '..')
const localesDir = join(web, 'src/i18n/locales')
const REFERENCE = 'en'
const PLURAL = /_(zero|one|two|few|many|other)$/
const TAG = /^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$/
const verbose = process.argv.includes('--verbose')

const errors = []
const fail = (msg) => errors.push(msg)

function flatten(node, file, prefix = '', out = new Map()) {
  for (const [k, v] of Object.entries(node)) {
    const key = prefix + k
    if (typeof v === 'string') out.set(key, v)
    else if (v && typeof v === 'object' && !Array.isArray(v)) flatten(v, file, `${key}.`, out)
    else fail(`${file}: ${key} is neither a message nor a group of them`)
  }
  return out
}

function load(tag) {
  const file = `src/i18n/locales/${tag}.json`
  try {
    return flatten(JSON.parse(readFileSync(join(web, file), 'utf8')), file)
  } catch (e) {
    fail(`${file}: ${e.message}`)
    return new Map()
  }
}

const placeholders = (msg) => [...msg.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort()
const tags = (msg) => [...msg.matchAll(/<(\w+)>/g)].map((m) => m[1]).sort()
const same = (a, b) => a.length === b.length && a.every((x, i) => x === b[i])
const base = (key) => key.replace(PLURAL, '')

const tagsFound = readdirSync(localesDir)
  .filter((f) => f.endsWith('.json'))
  .map((f) => f.slice(0, -'.json'.length))
for (const tag of tagsFound) {
  if (!TAG.test(tag)) fail(`src/i18n/locales/${tag}.json: the name must be a BCP 47 tag such as pt-BR`)
}
if (!tagsFound.includes(REFERENCE)) fail(`src/i18n/locales/${REFERENCE}.json is missing`)

const reference = load(REFERENCE)
const plurals = new Map()
for (const key of reference.keys()) {
  if (PLURAL.test(key)) plurals.set(base(key), [...(plurals.get(base(key)) ?? []), key])
}
for (const [key, forms] of plurals) {
  if (!forms.includes(`${key}_other`)) fail(`en.json: ${key} has plural forms but no ${key}_other`)
}
function expected(key) {
  const forms = plurals.get(base(key)) ?? [key]
  const msgs = forms.map((f) => reference.get(f)).filter((m) => m !== undefined)
  return {
    placeholders: [...new Set(msgs.flatMap(placeholders))].sort(),
    tags: [...new Set(msgs.flatMap(tags))].sort(),
  }
}

const known = new Set([...reference.keys()].map(base))
function sources(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name)
    if (statSync(path).isDirectory()) sources(path, out)
    else if (/\.(ts|tsx)$/.test(name) && !/\.test\.tsx?$/.test(name)) out.push(path)
  }
  return out
}
for (const file of sources(join(web, 'src'))) {
  const text = readFileSync(file, 'utf8')
  for (const m of text.matchAll(/\bt(?:\.rich)?\(\s*'([A-Za-z0-9_.]+)'/g)) {
    if (!known.has(m[1])) fail(`${relative(web, file)}: t('${m[1]}') is not a key in en.json`)
  }
}

const codesFile = join(web, '../internal/apierr/codes.go')
let codesText = null
try {
  codesText = readFileSync(codesFile, 'utf8')
} catch {}
const codes = []
if (codesText !== null) {
  for (const m of codesText.matchAll(/^\s*(Code\w+)\s+Code\b(.*)$/gm)) {
    const value = /^\s*=\s*"([a-z0-9_]+)"/.exec(m[2])
    if (value) codes.push(value[1])
    else fail(`internal/apierr/codes.go: declare ${m[1]} as \`${m[1]} Code = "snake_case"\`, one per line`)
  }
  if (codes.length === 0) fail('internal/apierr/codes.go: no error codes found')
}
for (const code of codes) {
  if (!known.has(`errors.${code}`))
    fail(`en.json: errors.${code} is missing, for the API error code ${code} (internal/apierr/codes.go)`)
}

const tagPatterns = [
  ['internal/auth/locale.go', /localeRe = regexp\.MustCompile\(`([^`]+)`\)/],
  ['web/src/i18n/index.tsx', /const SERVER_TAG = \/(.+)\/\n/],
  ['agent/src/language.ts', /const TAG = \/(.+)\/\n/],
]
const patterns = []
for (const [file, re] of tagPatterns) {
  let text
  try {
    text = readFileSync(join(web, '..', file), 'utf8')
  } catch {
    continue
  }
  const m = re.exec(text)
  if (m) patterns.push([file, m[1]])
  else fail(`${file}: the language tag pattern isn't where this check looks for it`)
}
for (const [file, pattern] of patterns.slice(1)) {
  if (pattern !== patterns[0][1])
    fail(`${file}: the language tag pattern ${pattern} differs from ${patterns[0][0]}'s ${patterns[0][1]}`)
}

const report = []
for (const tag of tagsFound.filter((t) => t !== REFERENCE).sort()) {
  const file = `${tag}.json`
  const messages = load(tag)
  const categories = new Intl.PluralRules(tag).resolvedOptions().pluralCategories
  for (const [key, msg] of messages) {
    const b = base(key)
    if (PLURAL.test(key) ? !plurals.has(b) : !reference.has(key)) {
      fail(`${file}: ${key} is not in en.json (renamed or removed there?)`)
      continue
    }
    const want = expected(key)
    const got = placeholders(msg).filter((p, i, all) => all.indexOf(p) === i)
    const extra = got.filter((p) => !want.placeholders.includes(p))
    if (extra.length) fail(`${file}: ${key} has {${extra.join('}, {')}}, which English doesn't fill in`)
    const missing = want.placeholders.filter((p) => !got.includes(p) && !(PLURAL.test(key) && p === 'count'))
    if (missing.length) fail(`${file}: ${key} leaves out {${missing.join('}, {')}}, which English shows`)
    if (!same([...new Set(tags(msg))].sort(), want.tags)) {
      fail(`${file}: ${key} has the tags <${tags(msg).join('>, <')}>, English <${want.tags.join('>, <')}>`)
    }
  }
  for (const key of plurals.keys()) {
    const have = categories.filter((c) => messages.has(`${key}_${c}`))
    if (have.length === 0) continue
    const missing = categories.filter((c) => c !== 'zero' && !messages.has(`${key}_${c}`))
    if (missing.length) fail(`${file}: ${key} lacks the plural forms ${missing.map((c) => `${key}_${c}`).join(', ')}`)
  }
  const untranslated = [...reference.keys()].filter((key) => {
    if (!PLURAL.test(key)) return !messages.has(key)
    return !categories.some((c) => messages.has(`${base(key)}_${c}`))
  })
  const unique = [...new Set(untranslated.map((k) => (PLURAL.test(k) ? base(k) : k)))]
  report.push({ tag, total: known.size, missing: unique })
}

console.log('Translations (English is the reference):')
for (const { tag, total, missing } of report) {
  const done = total - missing.length
  console.log(
    `  ${tag}: ${done} of ${total} messages translated${missing.length ? `, ${missing.length} ${missing.length === 1 ? 'shows' : 'show'} in English` : ''}`,
  )
  const shown = verbose ? missing : missing.slice(0, 10)
  for (const key of shown) console.log(`    untranslated: ${key}`)
  if (shown.length < missing.length)
    console.log(`    … and ${missing.length - shown.length} more (--verbose lists them)`)
}
if (errors.length) {
  console.error(`\n${errors.length} problem${errors.length === 1 ? '' : 's'}:`)
  for (const e of errors) console.error(`  ${e}`)
  process.exit(1)
}

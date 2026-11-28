import { readdirSync, readFileSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'
import { expect, test } from 'vitest'

const src = resolve(__dirname)

const SHELL_AND_SYNC = ['App.tsx', 'notifications/usePushSync.ts', 'shell/AccountMenu.tsx', 'shell/Shell.tsx']

function sources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) return sources(path)
    return /\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name) ? [path] : []
  })
}

function importedName(specifier: string): string {
  return (
    specifier
      .trim()
      .replace(/^type\s+/, '')
      .split(/\s+as\s+/)[0]
      ?.trim() ?? ''
  )
}

function importsModuleNavigate(source: string): boolean {
  if (/import\s*\*\s*as\s+\w+\s+from\s*'(?:\.\.?\/)+router'/.test(source)) return true
  const imports = source.matchAll(/import\s*\{([^}]*)\}\s*from\s*'(?:\.\.?\/)+router'/g)
  return [...imports].some((m) => (m[1] ?? '').split(',').some((name) => importedName(name) === 'navigate'))
}

test('only the shell, the app and the notification sync use the router’s own navigate', () => {
  const users = sources(src)
    .filter((file) => importsModuleNavigate(readFileSync(file, 'utf8')))
    .map((file) => relative(src, file))
    .sort()
  expect(users).toEqual(SHELL_AND_SYNC)
})

test('the scan sees a page that imports navigate, aliased or among others', () => {
  expect(importsModuleNavigate(`import { Link, navigate, routePath } from '../router'`)).toBe(true)
  expect(importsModuleNavigate(`import { Link, useNavigate } from '../router'`)).toBe(false)
  expect(importsModuleNavigate(`import { navigate } from './router'`)).toBe(true)
  expect(importsModuleNavigate(`import { Link, navigate as go, routePath } from '../router'`)).toBe(true)
  expect(importsModuleNavigate(`import {\n  navigate as go,\n} from '../../router'`)).toBe(true)
  expect(importsModuleNavigate(`import * as router from '../router'`)).toBe(true)
  expect(importsModuleNavigate(`import { useNavigate as navigate } from '../router'`)).toBe(false)
})

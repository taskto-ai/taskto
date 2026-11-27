import { readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, posix, relative, resolve } from 'node:path'
import { expect, test } from 'vitest'

const repo = resolve(__dirname, '../..')

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    if (statSync(path).isDirectory()) return sources(path)
    return /\.(ts|tsx)$/.test(name) ? [path] : []
  })
}

function copiedBesideWeb(dockerfile: string): string[] {
  type Copy = { src: string; dest: string }
  const stages: Copy[][] = []
  let workdir = '/'
  for (const line of dockerfile.split('\n')) {
    const [instruction, ...args] = line.trim().split(/\s+/)
    switch (instruction?.toUpperCase()) {
      case 'FROM':
        stages.push([])
        workdir = '/'
        break
      case 'WORKDIR':
        workdir = posix.resolve(workdir, args[0] ?? '.')
        break
      case 'COPY': {
        const paths = args.filter((a) => !a.startsWith('--'))
        if (args.some((a) => a.startsWith('--from=')) || paths.length !== 2) break
        const [src, dest] = paths.map((p) => p.replace(/\/$/, ''))
        stages.at(-1)?.push({ src, dest: posix.resolve(workdir, dest) })
      }
    }
  }
  return stages.flatMap((copies) => {
    const web = copies.find((c) => c.src === 'web')
    if (!web) return []
    const root = posix.dirname(web.dest)
    return copies.filter((c) => c.src.startsWith('internal/') && c.dest === posix.join(root, c.src)).map((c) => c.src)
  })
}

test('the image copies every directory of internal/ the web app imports', () => {
  const imported = new Set<string>()
  for (const file of sources(join(repo, 'web/src'))) {
    for (const [, spec] of readFileSync(file, 'utf8').matchAll(/from '((?:\.\.\/)+internal\/[^']+)'/g)) {
      imported.add(dirname(relative(repo, resolve(dirname(file), spec))))
    }
  }
  const copied = copiedBesideWeb(readFileSync(join(repo, 'Dockerfile'), 'utf8'))
  const missing = [...imported].filter((dir) => !copied.some((c) => dir === c || dir.startsWith(`${c}/`)))
  expect(imported.size).toBeGreaterThan(0)
  expect(missing).toEqual([])
})

test('a COPY counts only where the web app looks for it', () => {
  const stage = (lines: string) => `FROM node AS js\nWORKDIR /src\nCOPY web/ web/\n${lines}\nFROM golang AS go\n`
  expect(copiedBesideWeb(stage('COPY internal/apps/templates/ internal/apps/templates/'))).toEqual([
    'internal/apps/templates',
  ])
  expect(copiedBesideWeb(stage('COPY internal/apps/templates/ /src/internal/apps/templates/'))).toEqual([
    'internal/apps/templates',
  ])
  expect(copiedBesideWeb(stage('COPY internal/apps/templates/ /tmp/templates/'))).toEqual([])
  expect(copiedBesideWeb(stage('WORKDIR /other\nCOPY internal/apps/templates/ internal/apps/templates/'))).toEqual([])
  expect(copiedBesideWeb(`${stage('')}WORKDIR /src\nCOPY internal/apps/templates/ internal/apps/templates/\n`)).toEqual(
    [],
  )
})

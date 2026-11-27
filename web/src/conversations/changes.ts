import type { ChatMessage } from './transcript'

export type ChangeKind = 'created' | 'edited' | 'moved' | 'deleted' | 'restored' | 'archived' | 'commented'

export type Changed =
  | { kind: 'doc'; space: number; path: string; from?: string; change: ChangeKind }
  | { kind: 'card'; space: number; board: number; card: number; title: string; change: ChangeKind }
  | { kind: 'board'; space: number; board: number; name: string; change: ChangeKind }
  | { kind: 'app'; space: number; app: number; name: string; change: ChangeKind }
  | { kind: 'record'; space: number; app: number; record: number; appName?: string; change: ChangeKind }

type Fields = Record<string, unknown>

type Build = (args: Fields, result: Fields) => Changed | undefined

export const isId = (v: unknown): v is number => typeof v === 'number' && Number.isSafeInteger(v) && v > 0

const text = (v: unknown): string | undefined => (typeof v === 'string' && v !== '' ? v : undefined)

const fields = (v: unknown): Fields | undefined =>
  v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Fields) : undefined

const id = (...candidates: unknown[]): number | undefined => candidates.find(isId)

function docChange(change: (args: Fields) => ChangeKind, pathArg = 'path'): Build {
  return (args, result) => {
    const space = id(args.space_id)
    const path = text(fields(result.doc)?.path) ?? text(args[pathArg])
    if (!space || !path) return undefined
    const changed: Changed = { kind: 'doc', space, path, change: change(args) }
    const from = text(args.from)
    return pathArg === 'to' && from ? { ...changed, from } : changed
  }
}

function cardChange(change: ChangeKind): Build {
  return (args, result) => {
    const space = id(args.space_id)
    const board = id(result.board_id, args.board_id)
    const card = id(result.id, args.card_id)
    if (!space || !board || !card) return undefined
    return { kind: 'card', space, board, card, title: text(result.title) ?? '', change }
  }
}

const commentChange: Build = (args, result) => {
  const space = id(args.space_id)
  const board = id(args.board_id)
  const card = id(result.card_id, args.card_id)
  if (!space || !board || !card) return undefined
  return { kind: 'card', space, board, card, title: '', change: 'commented' }
}

function boardChange(change: ChangeKind): Build {
  return (args, result) => {
    const space = id(result.space_id, args.space_id)
    const board = id(result.id)
    if (!space || !board) return undefined
    return { kind: 'board', space, board, name: text(result.name) ?? '', change }
  }
}

function appChange(change: ChangeKind): Build {
  return (args, result) => {
    const space = id(result.space_id, args.space_id)
    const app = id(result.id)
    if (!space || !app) return undefined
    return { kind: 'app', space, app, name: text(result.name) ?? '', change }
  }
}

const RECORD_CHANGES: Record<string, ChangeKind> = { insert: 'created', update: 'edited', delete: 'deleted' }

const recordChange: Build = (args, result) => {
  const space = id(args.space_id)
  const app = id(args.app_id)
  const record = id(result.id)
  const kind = typeof result.kind === 'string' ? RECORD_CHANGES[result.kind] : undefined
  if (!space || !app || !record || !kind || result.changed !== true) return undefined
  return { kind: 'record', space, app, record, change: kind }
}

const BUILDERS: Record<string, Build> = {
  write_doc: docChange((args) => (text(args.base_version) ? 'edited' : 'created')),
  move_doc: docChange(() => 'moved', 'to'),
  delete_doc: docChange(() => 'deleted'),
  restore_doc: docChange(() => 'restored'),
  create_card: cardChange('created'),
  update_card: cardChange('edited'),
  move_card: cardChange('moved'),
  archive_card: cardChange('archived'),
  restore_card: cardChange('restored'),
  set_blocked: cardChange('edited'),
  comment_card: commentChange,
  create_board: boardChange('created'),
  update_board: boardChange('edited'),
  create_app: appChange('created'),
  update_app: appChange('edited'),
  install_app_template: appChange('created'),
  duplicate_app: appChange('created'),
  run_app_action: recordChange,
}

export const CHANGE_TOOLS: ReadonlySet<string> = new Set(Object.keys(BUILDERS))

export function changeOf(toolName: string, args: unknown, resultText: string): Changed | undefined {
  const build = Object.hasOwn(BUILDERS, toolName) ? BUILDERS[toolName] : undefined
  if (!build) return undefined
  try {
    const a = fields(args)
    const r = fields(JSON.parse(resultText))
    return a && r ? build(a, r) : undefined
  } catch {
    return undefined
  }
}

export function changeKey(c: Changed): string {
  switch (c.kind) {
    case 'doc':
      return `doc:${c.space}:${c.path}`
    case 'card':
      return `card:${c.space}:${c.board}:${c.card}`
    case 'board':
      return `board:${c.space}:${c.board}`
    case 'app':
      return `app:${c.space}:${c.app}`
    case 'record':
      return `record:${c.space}:${c.app}:${c.record}`
  }
}

function withName(next: Changed, prev: Changed): Changed {
  if (next.kind === 'card' && prev.kind === 'card' && !next.title) return { ...next, title: prev.title }
  if (next.kind === 'board' && prev.kind === 'board' && !next.name) return { ...next, name: prev.name }
  if (next.kind === 'app' && prev.kind === 'app' && !next.name) return { ...next, name: prev.name }
  return next
}

function merged(prev: Changed, next: Changed): Changed | null {
  if (prev.change === 'created') {
    if (next.change === 'deleted') return null
    if (next.change === 'archived') return withName(next, prev)
    return withName({ ...next, change: 'created' }, prev)
  }
  return withName(next, prev)
}

function moveDoc(list: Changed[], next: Extract<Changed, { kind: 'doc' }>) {
  const to = changeKey(next)
  const { from, ...target } = next
  const source = from === undefined ? -1 : list.findIndex((c) => changeKey(c) === changeKey({ ...target, path: from }))
  const rest = list.filter((c, i) => i === source || changeKey(c) !== to)
  const at = rest.indexOf(list[source])
  if (at < 0) {
    rest.push(target)
  } else {
    rest[at] = { ...target, change: list[source].change === 'created' ? 'created' : 'moved' }
  }
  list.splice(0, list.length, ...rest)
}

function mergeInto(list: Changed[], next: Changed) {
  if (next.kind === 'doc' && next.change === 'moved') return moveDoc(list, next)
  const key = changeKey(next)
  const at = list.findIndex((c) => changeKey(c) === key)
  if (at < 0) {
    list.push(next)
    return
  }
  const out = merged(list[at], next)
  if (out) list[at] = out
  else list.splice(at, 1)
}

function named(list: Changed[]): Changed[] {
  const apps = new Map<string, string>()
  for (const c of list) if (c.kind === 'app' && c.name) apps.set(`${c.space}:${c.app}`, c.name)
  return list.map((c) => {
    const name = c.kind === 'record' ? apps.get(`${c.space}:${c.app}`) : undefined
    return name ? { ...c, appName: name } : c
  })
}

export function turnChanges(messages: ChatMessage[]): Map<string, Changed[]> {
  const out = new Map<string, Changed[]>()
  let list: Changed[] = []
  let last: string | undefined
  const finish = () => {
    if (last && list.length > 0) out.set(last, named(list))
    list = []
    last = undefined
  }
  for (const m of messages) {
    if (m.role === 'user') {
      finish()
      continue
    }
    last = m.key
    for (const tool of m.tools) if (tool.changed) mergeInto(list, tool.changed)
  }
  finish()
  return out
}

import type { Comment } from '../api'
import type { MentionNames } from '../markdown'
import type { Person } from './people'

export function withComments(have: Comment[], read: Comment[]): Comment[] {
  const server = new Map(read.map((c) => [c.id, c]))
  let changed = false
  const merged = have.map((c) => {
    const fresh = server.get(c.id)
    if (!fresh || JSON.stringify(fresh) === JSON.stringify(c)) return c
    changed = true
    return fresh
  })
  const known = new Set(have.map((c) => c.id))
  const added = read.filter((c) => !known.has(c.id))
  return !changed && added.length === 0 ? have : [...merged, ...added].sort((a, b) => a.id - b.id)
}

export const mentionOf = (p: Person) => `@[${p.name.replace(/[[\]()\\\r\n]+/g, ' ').trim() || p.id}](user:${p.id})`

export const mentionNames = (c: Comment, unknown: string): MentionNames => ({
  names: new Map(c.mentions.map((m) => [String(m.id), m.name])),
  unknown,
})

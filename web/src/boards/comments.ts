import type { Comment } from '../api'
import type { MentionNames } from '../markdown'
import type { Person } from './people'

// A thread with a newer read's comments added, oldest first: new ones are added, and the read's copy
// replaces the held one with the same ID if it differs (the names its mentions have can change);
// comments held only here stay. The same array if nothing differs, so nothing renders again.
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

// A mention of someone in a comment (docs/api.md#mentions): the person's ID is what counts, and the
// name is only what is shown, so brackets, parentheses and line breaks can't be in it.
export const mentionOf = (p: Person) => `@[${p.name.replace(/[[\]()\\\r\n]+/g, ' ').trim() || p.id}](user:${p.id})`

// Who a comment's mentions are, by ID, as the server names them to this reader. An ID the server
// gave no name for (someone this reader can't see, or no one) shows as `unknown`, so the label in
// the text, which anyone can write, is never what is shown.
export const mentionNames = (c: Comment, unknown: string): MentionNames => ({
  names: new Map(c.mentions.map((m) => [String(m.id), m.name])),
  unknown,
})

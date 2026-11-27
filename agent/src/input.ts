export const NOTE_KIND = 'taskto.note'

export const MAX_NOTE_CHARS = 8000

export type NoteInput = { text: string; author: number; requestId?: string }

export class InputError extends Error {}

const REQUEST_ID = /^[\x20-\x7e]{0,128}$/

export function parseNote(body: Record<string, unknown>): NoteInput {
  const { text, author, requestId } = body
  if (typeof text !== 'string' || text.trim() === '') throw new InputError('text must be a non-empty string')
  if ([...text].length > MAX_NOTE_CHARS) throw new InputError(`text must be up to ${MAX_NOTE_CHARS} characters`)
  if (typeof author !== 'number' || !Number.isSafeInteger(author) || author < 1) {
    throw new InputError('author must be a positive integer')
  }
  if (requestId !== undefined && (typeof requestId !== 'string' || !REQUEST_ID.test(requestId))) {
    throw new InputError('requestId must be up to 128 printable ASCII characters')
  }
  return { text, author, ...(requestId === undefined ? {} : { requestId }) }
}

export function noteEntry(note: NoteInput) {
  return { kind: NOTE_KIND, data: { v: 1, author: note.author, text: note.text } }
}

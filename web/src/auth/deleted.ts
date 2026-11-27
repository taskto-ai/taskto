const KEY = 'taskto.accountDeleted'

export const DELETION_EVENT = 'taskto:account-deleted'

let inMemory: string | null = null
const shown = new Set<string>()
let droppedAll = false

let submissions = 0

function stored(): string | null {
  try {
    return window.sessionStorage.getItem(KEY) || null
  } catch {
    return null
  }
}

export function beginDeletion(): number {
  return submissions
}

export function rememberDeletion(purgeAt: string, since: number = submissions): void {
  if (since !== submissions) return
  inMemory = purgeAt
  shown.delete(purgeAt)
  droppedAll = false
  try {
    window.sessionStorage.setItem(KEY, purgeAt)
  } catch {}
  window.dispatchEvent(new Event(DELETION_EVENT))
}

export function peekDeletion(): string | null {
  if (inMemory !== null) return inMemory
  const kept = stored()
  return kept !== null && !droppedAll && !shown.has(kept) ? kept : null
}

export function ackDeletion(shownDate?: string): void {
  if (shownDate !== undefined && inMemory !== null && inMemory !== shownDate) return
  const superseded = inMemory !== null
  const gone = shownDate ?? peekDeletion()
  inMemory = null
  if (shownDate === undefined) droppedAll = true
  if (gone !== null) shown.add(gone)
  const kept = stored()
  if (kept !== null && superseded) shown.add(kept)
  if (shownDate !== undefined && !superseded && kept !== null && kept !== shownDate) return
  try {
    window.sessionStorage.removeItem(KEY)
  } catch {
    try {
      window.sessionStorage.setItem(KEY, '')
    } catch {}
  }
}

export function signInSubmitted(): void {
  submissions++
  ackDeletion()
}

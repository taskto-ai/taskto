import { type ConversationId, type Cursor, defineDoc, type EntryId, type Tx } from '@earendil-works/pi-durable'
import { NOTE_KIND } from './input.js'

type ContinueState = { consumed: number; submission: number }

export const ContinuePin = defineDoc<ContinueState>({
  kind: 'taskto.continue-pin',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ consumed: 0, submission: 0 }),
  checkpointWhen: () => true,
})

export function pinHolds(pin: ContinueState | undefined, live: { run?: { inputs: readonly number[] } } | undefined) {
  return (
    pin !== undefined && pin.submission !== 0 && live?.run?.inputs.some((id) => Number(id) === pin.submission) === true
  )
}

const SCAN_PAGE = 50

export async function latestReplyEntryId(tx: Tx, conversation: ConversationId): Promise<EntryId | undefined> {
  let cursor: Cursor | undefined
  do {
    const page = await tx.scanEntries({ conversationId: conversation, order: 'descending' }, SCAN_PAGE, cursor)
    const found = page.items.find((entry) => entry.kind !== NOTE_KIND)
    if (found) return found.id
    cursor = page.next
  } while (cursor !== undefined)
  return undefined
}

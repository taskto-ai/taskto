// What the server needs to know of a conversation besides Pi Durable's record: when it was created,
// so a sweep for orphans leaves alone one the server is still recording (0 for one created before
// this document existed), and when it was retired. The purge (purge.ts) recognizes a retired
// conversation by this document, so it is the last document of a conversation the purge deletes.

import type { Context } from '@earendil-works/chord/context'
import { type ConversationId, defineDoc, type Harness } from '@earendil-works/pi-durable'
import { LIFECYCLE_DOC_KIND } from './purge.js'

export const Lifecycle = defineDoc<{ createdAt: number; retiredAt: number | null }>({
  kind: LIFECYCLE_DOC_KIND,
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ createdAt: 0, retiredAt: null }),
  checkpointWhen: () => true, // it changes at most twice
})

/** Where a conversation stands for the purge's route: not there, there and in use, or retired. */
export async function conversationState(
  harness: Harness,
  id: ConversationId,
  context: Context,
): Promise<'unknown' | 'live' | 'retired'> {
  if (!(await harness.conversation(id, context))) return 'unknown'
  return (await harness.snapshot(Lifecycle, id, context))?.retiredAt == null ? 'live' : 'retired'
}

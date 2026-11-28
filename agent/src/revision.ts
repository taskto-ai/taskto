import { type ConversationId, configure, defineDoc, type Tx } from '@earendil-works/pi-durable'

type RevisionState = { n: number }

export const ModelRevision = defineDoc<RevisionState>({
  kind: 'taskto.model-revision',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ n: 0 }),
  checkpointWhen: () => true,
})

export type ModelValue = { provider: string; modelId: string }

export async function modelRevision(tx: Tx, conversation: ConversationId): Promise<number> {
  return (await tx.doc(ModelRevision, conversation)).n
}

export async function writeModel(tx: Tx, conversation: ConversationId, model: ModelValue): Promise<number> {
  await configure(tx, conversation, { model })
  const revision = await tx.doc(ModelRevision, conversation)
  revision.n += 1
  return revision.n
}

export function pastDeadline(admitBy: number | undefined, now: number = Date.now()): boolean {
  return admitBy !== undefined && now > admitBy
}

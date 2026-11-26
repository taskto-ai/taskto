// What the server needs to know of a conversation besides Pi Durable's record: when it was created,
// so a sweep for orphans leaves alone one the server is still recording (0 for one created before
// this document existed), and when it was retired. Its own module, so the tests use the document
// the sidecar uses.
import { defineDoc } from '@earendil-works/pi-durable'
import { LIFECYCLE_KIND } from './purge.js'

export const Lifecycle = defineDoc<{ createdAt: number; retiredAt: number | null }>({
  kind: LIFECYCLE_KIND, // the purge removes the root's last (purge.ts)
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ createdAt: 0, retiredAt: null }),
  checkpointWhen: () => true, // it changes at most twice
})

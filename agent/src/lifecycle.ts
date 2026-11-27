import { defineDoc } from '@earendil-works/pi-durable'
import { LIFECYCLE_KIND } from './purge.js'

export const Lifecycle = defineDoc<{ createdAt: number; retiredAt: number | null }>({
  kind: LIFECYCLE_KIND,
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ createdAt: 0, retiredAt: null }),
  checkpointWhen: () => true,
})

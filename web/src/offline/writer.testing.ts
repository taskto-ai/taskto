// For tests of the screens and the engine that have one tab: a writer that always holds the right,
// and that no store is attached to (nothing is refused), so a test of its own that has several
// tabs uses the real `OfflineWriter`.

import type { OfflineWriter } from './writer'

export const soleWriter = {
  owner: 'sole',
  holds: () => true,
  session: () => 1,
  subscribe: () => () => {},
  takeOver: async () => {},
  start: async () => {},
  stop: () => {},
} as unknown as OfflineWriter

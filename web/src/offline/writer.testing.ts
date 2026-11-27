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

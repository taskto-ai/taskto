// Time zones: the person's own is kept on their account (docs/api.md, `PATCH /api/me`), and what the
// server times for them (card reminders, the daily email summary) follows it. This browser's is the
// first guess, sent once for an account that has none.

import { api, forAccount, type Me } from './api'

// The IANA name of this browser's time zone, or "" if it won't say.
export const browserTimeZone = (): string => Intl.DateTimeFormat().resolvedOptions().timeZone || ''

// The zones the browser knows, UTC first, and `current` if it isn't among them (a name the account
// has that this browser doesn't know).
export function timeZones(current: string): string[] {
  const known =
    (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.('timeZone') ?? []
  const all = new Set<string>(['UTC', ...known])
  if (current) all.add(current)
  return [...all]
}

// An account with no time zone (`user.time_zone` is "": servers that don't send the field aren't asked)
// gets this browser's, once: the server fills an empty zone only, so a zone the person picked, here or
// in another browser, is never replaced. The answer is the account as it is then. Never a reason to fail
// the page: if the server won't take it, the account stays as it was and the next load tries again.
export async function reportTimeZone(me: Me): Promise<Me> {
  const zone = browserTimeZone()
  if (me.user.time_zone !== '' || !zone) return me
  try {
    // Named for its account: a start whose cookie is someone else's by now changes nothing of theirs.
    return await api<Me>('/api/me', {
      method: 'PATCH',
      body: { browser_time_zone: zone },
      headers: forAccount(me.user.id),
    })
  } catch {
    return me
  }
}

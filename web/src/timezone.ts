import { api, forAccount, type Me } from './api'

export const browserTimeZone = (): string => Intl.DateTimeFormat().resolvedOptions().timeZone || ''

export function timeZones(current: string): string[] {
  const known =
    (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.('timeZone') ?? []
  const all = new Set<string>(['UTC', ...known])
  if (current) all.add(current)
  return [...all]
}

export async function reportTimeZone(me: Me): Promise<Me> {
  const zone = browserTimeZone()
  if (me.user.time_zone !== '' || !zone) return me
  try {
    return await api<Me>('/api/me', {
      method: 'PATCH',
      body: { browser_time_zone: zone },
      headers: forAccount(me.user.id),
    })
  } catch {
    return me
  }
}

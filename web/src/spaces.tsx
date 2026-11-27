import { useState } from 'react'
import { api, type Space, ws } from './api'
import { t } from './i18n'
import { useLoad } from './load'

export function useSpaces(workspace: number, initial?: number) {
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  const [chosen, setChosen] = useState<number | null>(initial ?? null)
  const space = spaces.data?.find((s) => s.id === chosen) ?? spaces.data?.[0]
  return { spaces, space, choose: setChosen }
}

export function spaceLabel(s: Space): string {
  return s.kind === 'private' ? t('knowledge.spaces.private') : s.name
}

// Picking one of the spaces the user can read, for the pages whose content lives in spaces.

import { useState } from 'react'
import { api, type Space, ws } from './api'
import { t } from './i18n'
import { useLoad } from './load'

// initial is the space to open on (a link's), if the user can read it.
export function useSpaces(workspace: number, initial?: number) {
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  const [chosen, setChosen] = useState<number | null>(initial ?? null)
  const space = spaces.data?.find((s) => s.id === chosen) ?? spaces.data?.[0]
  return { spaces, space, choose: setChosen }
}

// A space's name; a private space's is said in the user's language. A component showing it
// also calls useT(), so it re-renders when the language changes.
export function spaceLabel(s: Space): string {
  return s.kind === 'private' ? t('knowledge.spaces.private') : s.name
}

// Picking one of the spaces the user can read, for the pages whose content lives in spaces.

import { useState } from 'react'
import { api, type Space, ws } from './api'
import { t, useT } from './i18n'
import { useLoad } from './load'

export function useSpaces(workspace: number) {
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  const [chosen, setChosen] = useState<number | null>(null)
  const space = spaces.data?.find((s) => s.id === chosen) ?? spaces.data?.[0]
  return { spaces, space, choose: setChosen }
}

// A space's name; a private space's is said in the user's language. A component showing it
// also calls useT(), so it re-renders when the language changes.
export function spaceLabel(s: Space): string {
  return s.kind === 'private' ? t('knowledge.spaces.private') : s.name
}

export function SpacePicker({
  spaces,
  space,
  choose,
}: {
  spaces: Space[]
  space: Space | undefined
  choose: (id: number) => void
}) {
  const t = useT()
  if (spaces.length < 2) return null
  return (
    <label className="tt-inline-field">
      {t('knowledge.spaces.label')}
      <select value={space?.id} onChange={(e) => choose(Number(e.target.value))}>
        {spaces.map((s) => (
          <option key={s.id} value={s.id}>
            {spaceLabel(s)}
          </option>
        ))}
      </select>
    </label>
  )
}

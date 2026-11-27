import { createContext, useContext, useEffect, useId } from 'react'

export interface Drafts {
  set(part: string, unsaved: boolean): void
  any(): boolean
}

export function makeDrafts(): Drafts {
  const unsaved = new Set<string>()
  return {
    set: (part, on) => {
      if (on) unsaved.add(part)
      else unsaved.delete(part)
    },
    any: () => unsaved.size > 0,
  }
}

export const DraftsContext = createContext<Drafts | null>(null)

export function useDraft(unsaved: boolean) {
  const drafts = useContext(DraftsContext)
  const part = useId()
  useEffect(() => {
    drafts?.set(part, unsaved)
    return () => drafts?.set(part, false)
  }, [drafts, part, unsaved])
}

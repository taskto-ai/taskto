import { createContext, useContext, useEffect, useId } from 'react'

// What the open card holds that isn't saved or sent yet: a description being edited, a comment
// being written, a label, a reason or a path typed into a form. Each part says whether it holds
// one; the card asks before it closes over any of them.
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

// Tells the open card whether this part holds an unsaved draft; a part that goes holds none.
export function useDraft(unsaved: boolean) {
  const drafts = useContext(DraftsContext)
  const part = useId()
  useEffect(() => {
    drafts?.set(part, unsaved)
    return () => drafts?.set(part, false)
  }, [drafts, part, unsaved])
}

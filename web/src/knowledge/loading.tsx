// "Loading…" said to a screen reader through one polite status region that is always there (the
// page's), not by text inserted with each skeleton: a live region that appears with its words,
// or one marked busy, may never be read. Each part that waits says so with useLoadingStatus; the
// skeletons themselves are only shapes, hidden from screen readers.

import { createContext, type ReactNode, useCallback, useContext, useId, useLayoutEffect, useState } from 'react'
import { useT } from '../i18n'
import { VisuallyHidden } from '../ui/VisuallyHidden'

type Report = (part: string, loading: boolean) => void

const LoadingContext = createContext<Report>(() => {})

export function LoadingStatus({ children }: { children: ReactNode }) {
  const t = useT()
  const [waiting, setWaiting] = useState<ReadonlySet<string>>(new Set())
  const report = useCallback<Report>(
    (part, loading) =>
      setWaiting((was) => {
        if (was.has(part) === loading) return was
        const next = new Set(was)
        if (loading) next.add(part)
        else next.delete(part)
        return next
      }),
    [],
  )
  return (
    <LoadingContext.Provider value={report}>
      {children}
      <VisuallyHidden>
        <span role="status">{waiting.size > 0 ? t('app.loading') : ''}</span>
      </VisuallyHidden>
    </LoadingContext.Provider>
  )
}

// This part is waiting for what it shows (while `loading`), until it is done or goes away.
export function useLoadingStatus(loading: boolean) {
  const part = useId()
  const report = useContext(LoadingContext)
  // Before the screen is painted, so the region never says "Loading…" of what has come.
  useLayoutEffect(() => {
    report(part, loading)
    return () => report(part, false)
  }, [part, loading, report])
}

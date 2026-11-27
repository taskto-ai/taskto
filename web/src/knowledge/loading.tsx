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

export function useLoadingStatus(loading: boolean) {
  const part = useId()
  const report = useContext(LoadingContext)
  useLayoutEffect(() => {
    report(part, loading)
    return () => report(part, false)
  }, [part, loading, report])
}

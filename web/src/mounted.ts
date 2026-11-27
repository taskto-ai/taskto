import { useLayoutEffect, useRef } from 'react'

export function useMounted() {
  const mounted = useRef(true)
  useLayoutEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  return mounted
}

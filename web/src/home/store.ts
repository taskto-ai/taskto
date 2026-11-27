import { type DependencyList, useEffect, useRef, useSyncExternalStore } from 'react'
import { type Loaded, useLoad } from '../load'

let version = 0
const listeners = new Set<() => void>()

export function homeChanged() {
  version++
  for (const listener of listeners) listener()
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

export function homeVersion(): number {
  return version
}

export function useHomeVersion(): number {
  return useSyncExternalStore(subscribe, () => version)
}

export function useHomeLoad<T>(load: () => Promise<T>, deps: DependencyList): Loaded<T> {
  const loaded = useLoad(load, deps)
  const version = useHomeVersion()
  const seen = useRef(version)
  const { reload } = loaded
  useEffect(() => {
    if (seen.current === version) return
    seen.current = version
    reload()
  }, [version, reload])
  return loaded
}

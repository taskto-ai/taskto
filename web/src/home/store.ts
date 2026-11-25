// Home's placements are shown in two places that load them on their own: the Home page and the pins
// in the navigation. A change made in one tells the other to load again.

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

// The version now: a load started at it shows every change made before it.
export function homeVersion(): number {
  return version
}

// A number that changes whenever Home's placements did: a dependency to load them again on.
export function useHomeVersion(): number {
  return useSyncExternalStore(subscribe, () => version)
}

// Loads what Home shows, and loads it again when Home's placements change, keeping what is shown
// until the answer comes: a change made in customize mode doesn't empty the page under the person
// (and the keyboard, and the scroll position) while it reloads.
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

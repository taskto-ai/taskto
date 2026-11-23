// Home's placements are shown in two places that load them on their own: the Home page and the pins
// in the navigation. A change made in one tells the other to load again.

import { useSyncExternalStore } from 'react'

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

// A number that changes whenever Home's placements did: a dependency to load them again on.
export function useHomeVersion(): number {
  return useSyncExternalStore(subscribe, () => version)
}

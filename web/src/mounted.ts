// Whether the part that made a request is still on the screen when its answer comes. A screen the
// person left, or one that went with their session (logged out, the session lost), does nothing a
// person would see or keep with the answer: no toast, no change of address, no preference written.
// What the server did is done either way; the next screen loads it again.

import { useEffect, useRef } from 'react'

export function useMounted() {
  const mounted = useRef(true)
  // Set again on each mount, so StrictMode's second mount finds it true.
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  return mounted
}

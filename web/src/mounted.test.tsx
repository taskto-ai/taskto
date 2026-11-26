// @vitest-environment jsdom

// useMounted() says false as soon as React has taken the screen away (its commit), not later when
// React gets to the passive effects: an answer that arrives in between must already see it gone.

import { useState } from 'react'
import { createRoot } from 'react-dom/client'
import { afterEach, expect, test } from 'vitest'
import { useMounted } from './mounted'

const env = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }
const before = env.IS_REACT_ACT_ENVIRONMENT
afterEach(() => {
  env.IS_REACT_ACT_ENVIRONMENT = before
})

test('a screen taken away reads as gone from the moment it leaves the page', async () => {
  // React as a browser runs it, not as act() does (which flushes everything at once).
  env.IS_REACT_ACT_ENVIRONMENT = false
  const seen: { mounted: { readonly current: boolean } | null } = { mounted: null }
  let hide: () => void = () => {}
  function Child() {
    seen.mounted = useMounted()
    return <p id="child">here</p>
  }
  function Parent() {
    const [shown, setShown] = useState(true)
    hide = () => setShown(false)
    return shown ? <Child /> : null
  }
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  root.render(<Parent />)
  await new Promise((r) => setTimeout(r, 20))
  expect(seen.mounted?.current).toBe(true)

  // The moment the child leaves the page (a microtask after React's commit, before its passive
  // effects run): what does the screen's ref say?
  const atRemoval = new Promise<boolean | undefined>((resolve) => {
    const watch = new MutationObserver(() => {
      if (!host.querySelector('#child')) {
        watch.disconnect()
        resolve(seen.mounted?.current)
      }
    })
    watch.observe(host, { childList: true, subtree: true })
  })
  hide()
  expect(await atRemoval).toBe(false)
  root.unmount()
  host.remove()
})

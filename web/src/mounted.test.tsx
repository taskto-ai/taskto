// @vitest-environment jsdom

import { useState } from 'react'
import { createRoot } from 'react-dom/client'
import { afterEach, expect, test } from 'vitest'
import { useMounted } from './mounted'

const env = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }
const before = env.IS_REACT_ACT_ENVIRONMENT
afterEach(() => {
  env.IS_REACT_ACT_ENVIRONMENT = before
})

const seen: { mounted: { readonly current: boolean } | null; hide: () => void } = { mounted: null, hide: () => {} }

function Child() {
  seen.mounted = useMounted()
  return <p id="child">here</p>
}

function Parent() {
  const [shown, setShown] = useState(true)
  seen.hide = () => setShown(false)
  return shown ? <Child /> : null
}

test('a screen taken away reads as gone from the moment it leaves the page', async () => {
  env.IS_REACT_ACT_ENVIRONMENT = false
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  root.render(<Parent />)
  await new Promise((r) => setTimeout(r, 20))
  expect(seen.mounted?.current).toBe(true)

  const atRemoval = new Promise<boolean | undefined>((resolve) => {
    const watch = new MutationObserver(() => {
      if (!host.querySelector('#child')) {
        watch.disconnect()
        resolve(seen.mounted?.current)
      }
    })
    watch.observe(host, { childList: true, subtree: true })
  })
  seen.hide()
  expect(await atRemoval).toBe(false)
  root.unmount()
  host.remove()
})

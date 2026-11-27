// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { Component, type ReactNode } from 'react'
import { afterEach, expect, test, vi } from 'vitest'
import { httpTransport } from './transport'

vi.mock('./AppView', () => ({
  AppView: ({ label }: { label: string }) => {
    if (label === 'Broken') throw new Error('the app failed while it was drawn')
    return <p>{`drawn: ${label}`}</p>
  },
}))

async function fresh() {
  vi.resetModules()
  const mod = await import('./LazyAppView')
  return { ...mod, real: mod.rendererLoad.import }
}
afterEach(cleanup)

const props = (label: string) => ({
  surfaceId: 's',
  label,
  transport: httpTransport('/api/x'),
  frame: 'bare' as const,
})

class Above extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false }
  static getDerivedStateFromError() {
    return { failed: true }
  }
  render() {
    return this.state.failed ? <p>the boundary above</p> : this.props.children
  }
}

test('while the renderer comes, the app’s frame shows its loading shape, said to a screen reader', async () => {
  const { LazyAppView, rendererLoad, real } = await fresh()
  let arrive: () => void = () => {}
  rendererLoad.import = () =>
    new Promise((resolve) => {
      arrive = () => resolve(real())
    }) as ReturnType<typeof real>
  render(<LazyAppView {...props('Field visit')} />)
  const frame = await screen.findByRole('region', { name: 'Field visit' })
  expect(frame.getAttribute('data-frame')).toBe('bare')
  expect(within(frame).getByRole('status').textContent).toBe('Loading…')
  arrive()
  expect(await screen.findByText('drawn: Field visit')).toBeTruthy()
})

test('a renderer that doesn’t arrive is said in the app’s own frame, and Try again imports anew', async () => {
  const { LazyAppView, rendererLoad, real } = await fresh()
  const attempts = vi.fn()
  let fail = true
  rendererLoad.import = () => {
    attempts()
    return fail ? Promise.reject(new TypeError('Failed to fetch dynamically imported module')) : real()
  }
  render(
    <Above>
      <LazyAppView {...props('Field visit')} />
    </Above>,
  )
  const said = await screen.findByText(/This screen couldn't be loaded/)
  const frame = screen.getByRole('region', { name: 'Field visit' })
  expect(frame.contains(said)).toBe(true)
  expect(frame.getAttribute('data-frame')).toBe('bare')
  expect(screen.queryByText('the boundary above')).toBeNull()
  fail = false
  fireEvent.click(within(frame).getByRole('button', { name: 'Try again' }))
  expect(await screen.findByText('drawn: Field visit')).toBeTruthy()
  expect(attempts).toHaveBeenCalledTimes(2)
})

test('after a failure, a later draw elsewhere imports again rather than keeping the failure', async () => {
  const { LazyAppView, rendererLoad, real } = await fresh()
  let fail = true
  rendererLoad.import = () => (fail ? Promise.reject(new TypeError('Failed to fetch')) : real())
  const { unmount } = render(<LazyAppView {...props('First')} />)
  expect(await screen.findByText(/This screen couldn't be loaded/)).toBeTruthy()
  unmount()
  fail = false
  render(<LazyAppView {...props('Second')} />)
  expect(await screen.findByText('drawn: Second')).toBeTruthy()
})

test('an app that fails while it is drawn is the boundary above’s to say, not a missing renderer', async () => {
  const { LazyAppView, rendererLoad, real } = await fresh()
  rendererLoad.import = real
  render(
    <Above>
      <LazyAppView {...props('Broken')} />
    </Above>,
  )
  expect(await screen.findByText('the boundary above')).toBeTruthy()
  expect(screen.queryByText(/This screen couldn't be loaded/)).toBeNull()
})

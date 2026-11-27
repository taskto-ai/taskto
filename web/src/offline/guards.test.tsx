// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { beginSession, endSession } from '../session'
import { AccountChanged } from './AccountChanged'
import { ForeignGuard } from './ForeignGuard'
import { LogoutGuard } from './LogoutGuard'
import type { Runtime } from './runtime'
import { soleWriter } from './writer.testing'

afterEach(cleanup)

const pressEscape = (dialog: HTMLElement) => {
  const cancel = new Event('cancel', { cancelable: true })
  act(() => {
    dialog.dispatchEvent(cancel)
  })
  return cancel
}

test('another account’s records: no way past but an answer, signing out focused, discarding asked again', () => {
  const onDiscard = vi.fn()
  const onSignOut = vi.fn()
  render(<ForeignGuard count={2} onDiscard={onDiscard} onSignOut={onSignOut} />)
  const dialog = screen.getByRole('alertdialog', { name: 'Records of another account' })
  expect(dialog.textContent).toMatch(/2 unsent records of another account/)
  expect(within(dialog).queryByRole('button', { name: 'Close' })).toBeNull()
  expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Sign out to sign in as that account' }))
  expect(pressEscape(dialog).defaultPrevented).toBe(true)
  expect(screen.getByRole('alertdialog', { name: 'Records of another account' })).toBe(dialog)

  fireEvent.click(screen.getByRole('button', { name: 'Discard them' }))
  const question = screen.getByRole('alertdialog', { name: 'Discard them' })
  expect(question.textContent).toMatch(/Discard 2 records that were never sent/)
  expect(document.activeElement).toBe(within(question).getByRole('button', { name: 'Cancel' }))
  fireEvent.click(within(question).getByRole('button', { name: 'Cancel' }))
  expect(onDiscard).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: 'Discard them' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  expect(onDiscard).toHaveBeenCalledOnce()
  expect(onSignOut).not.toHaveBeenCalled()
})

test('the account changed: one answer, reloading, focused, and nothing closes it', () => {
  render(<AccountChanged />)
  const dialog = screen.getByRole('alertdialog', { name: 'The account changed' })
  expect(within(dialog).queryByRole('button', { name: 'Close' })).toBeNull()
  expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Reload this page' }))
  pressEscape(dialog)
  expect(screen.getByRole('alertdialog', { name: 'The account changed' })).toBe(dialog)
})

test('signing out with no database open here: Sync is off, Cancel is focused, Escape is Cancel, discarding is asked again', () => {
  const onSignOut = vi.fn()
  const onCancel = vi.fn()
  render(<LogoutGuard runtime={null} count={3} onSignOut={onSignOut} onCancel={onCancel} />)
  const dialog = screen.getByRole('alertdialog', { name: 'Records not sent yet' })
  expect(dialog.textContent).toMatch(/3 records on this device haven’t reached the server/)
  expect((screen.getByRole('button', { name: 'Sync now' }) as HTMLButtonElement).disabled).toBe(true)
  expect(document.activeElement).toBe(within(dialog).getByRole('button', { name: 'Cancel' }))
  pressEscape(dialog)
  expect(onCancel).toHaveBeenCalledOnce()
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(
    within(screen.getByRole('alertdialog', { name: 'Discard and sign out' })).getByRole('button', { name: 'Cancel' }),
  )
  expect(onSignOut).not.toHaveBeenCalled()
})

test('a database that can’t be checked offers to keep it, focused, before discarding', () => {
  const onKeep = vi.fn()
  render(
    <LogoutGuard
      runtime={null}
      count={0}
      unreadable="schema"
      onSignOut={() => {}}
      onKeep={onKeep}
      onCancel={() => {}}
    />,
  )
  const keep = screen.getByRole('button', { name: 'Keep them and sign out' })
  expect(document.activeElement).toBe(keep)
  fireEvent.click(keep)
  expect(onKeep).toHaveBeenCalledOnce()
})

test('signing out with unsent records and the database open: Sync now is the focused answer, and sends them', async () => {
  const onSignOut = vi.fn()
  const retry = vi.fn(async () => {})
  const runtime = {
    user: 7,
    writer: soleWriter,
    engine: { retry },
    store: { unsynced: async () => [] },
    stop: () => {},
  } as unknown as Runtime
  render(<LogoutGuard runtime={runtime} count={1} onSignOut={onSignOut} onCancel={() => {}} />)
  const sync = screen.getByRole('button', { name: 'Sync now' }) as HTMLButtonElement
  expect(sync.disabled).toBe(false)
  expect(document.activeElement).toBe(sync)
  fireEvent.click(sync)
  await vi.waitFor(() => expect(onSignOut).toHaveBeenCalledOnce())
  expect(retry).toHaveBeenCalledOnce()
})

function pendingSync() {
  let finish: () => void = () => {}
  const retry = vi.fn(
    () =>
      new Promise<void>((resolve) => {
        finish = resolve
      }),
  )
  const runtime = {
    user: 7,
    writer: soleWriter,
    engine: { retry },
    store: { unsynced: async () => [] },
    stop: () => {},
  } as unknown as Runtime
  return { runtime, retry, finish: () => finish() }
}

test('while a sync runs the guard can’t be closed: no close button, and Escape does nothing', async () => {
  const onSignOut = vi.fn()
  const onCancel = vi.fn()
  const sync = pendingSync()
  render(<LogoutGuard runtime={sync.runtime} count={1} onSignOut={onSignOut} onCancel={onCancel} />)
  const dialog = screen.getByRole('alertdialog', { name: 'Records not sent yet' })
  fireEvent.click(screen.getByRole('button', { name: 'Sync now' }))
  await vi.waitFor(() => expect(sync.retry).toHaveBeenCalledOnce())
  expect(within(dialog).queryByRole('button', { name: 'Close' })).toBeNull()
  pressEscape(dialog)
  expect(onCancel).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: /Sync now/ }))
  expect(sync.retry).toHaveBeenCalledOnce()
  await act(async () => sync.finish())
  await vi.waitFor(() => expect(onSignOut).toHaveBeenCalledOnce())
  pressEscape(dialog)
  expect(onCancel).toHaveBeenCalledOnce()
})

test('a guard that goes away while its sync runs signs nobody out when the sync ends', async () => {
  const onSignOut = vi.fn()
  const sync = pendingSync()
  const { unmount } = render(<LogoutGuard runtime={sync.runtime} count={1} onSignOut={onSignOut} onCancel={() => {}} />)
  fireEvent.click(screen.getByRole('button', { name: 'Sync now' }))
  await vi.waitFor(() => expect(sync.retry).toHaveBeenCalledOnce())
  unmount()
  await act(async () => {
    sync.finish()
    await new Promise((r) => setTimeout(r))
  })
  expect(onSignOut).not.toHaveBeenCalled()
})

test('a guard whose session ends while its sync runs (another account in another tab) signs nobody out', async () => {
  const onSignOut = vi.fn()
  const sync = pendingSync()
  beginSession(1)
  render(<LogoutGuard runtime={sync.runtime} count={1} onSignOut={onSignOut} onCancel={() => {}} />)
  fireEvent.click(screen.getByRole('button', { name: 'Sync now' }))
  await vi.waitFor(() => expect(sync.retry).toHaveBeenCalledOnce())
  endSession()
  await act(async () => {
    sync.finish()
    await new Promise((r) => setTimeout(r))
  })
  expect(onSignOut).not.toHaveBeenCalled()
})

test('a guard that goes away while this tab asks for the right to send sends nothing when it gets it', async () => {
  let granted: () => void = () => {}
  let holds = false
  const retry = vi.fn(async () => {})
  const runtime = {
    user: 7,
    writer: {
      holds: () => holds,
      takeOver: () =>
        new Promise<void>((resolve) => {
          granted = () => {
            holds = true
            resolve()
          }
        }),
    },
    engine: { retry },
    store: { unsynced: async () => [] },
    stop: () => {},
  } as unknown as Runtime
  const { unmount } = render(<LogoutGuard runtime={runtime} count={1} onSignOut={() => {}} onCancel={() => {}} />)
  fireEvent.click(screen.getByRole('button', { name: 'Sync now' }))
  unmount()
  await act(async () => {
    granted()
    await new Promise((r) => setTimeout(r))
  })
  expect(retry).not.toHaveBeenCalled()
})

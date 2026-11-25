// @vitest-environment jsdom

// The three guards as dialogs: what is at stake said, the safe answer focused, the destructive one
// asked again, and Escape doing only what Cancel does (or nothing, where there is no Cancel).

import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { AccountChanged } from './AccountChanged'
import { ForeignGuard } from './ForeignGuard'
import { LogoutGuard } from './LogoutGuard'

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

  // Discarding asks first; Cancel there keeps them.
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

test('signing out with unsent records: Sync now is focused, Escape is Cancel, discarding is asked again', () => {
  const onSignOut = vi.fn()
  const onCancel = vi.fn()
  render(<LogoutGuard runtime={null} count={3} onSignOut={onSignOut} onCancel={onCancel} />)
  const dialog = screen.getByRole('alertdialog', { name: 'Records not sent yet' })
  expect(dialog.textContent).toMatch(/3 records on this device haven’t reached the server/)
  // With no database open here there is nothing to sync, and it says so by being unavailable.
  expect((screen.getByRole('button', { name: 'Sync now' }) as HTMLButtonElement).disabled).toBe(true)
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

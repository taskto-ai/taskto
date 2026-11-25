// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import { Dialog } from './Dialog'
import { ANNOUNCE_MS, dismissToast, resetToasts, Toaster, toast } from './Toast'

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  act(() => resetToasts())
  cleanup()
  vi.useRealTimers()
})

const polite = () => document.querySelector('[aria-live="polite"]') as HTMLElement
const assertive = () => document.querySelector('[aria-live="assertive"]') as HTMLElement
const stack = () => document.querySelector('.ui-toaster') as HTMLElement
const shown = () => within(stack())

describe('Toast', () => {
  test('the live regions are on the page, empty, before a toast goes in', () => {
    render(<Toaster />)
    expect(polite().textContent).toBe('')
    expect(assertive().textContent).toBe('')
    act(() => {
      toast('Link copied')
    })
    expect(polite().textContent).toBe('Link copied')
    expect(shown().getByText('Link copied')).toBeTruthy()
    expect(stack().hasAttribute('data-popover-open')).toBe(true)
  })

  test('the toasts you see are not a live region; what is said is said once, then the announcer is empty', () => {
    render(<Toaster />)
    expect(stack().closest('[aria-live]')).toBeNull()
    expect(stack().querySelector('[aria-live]')).toBeNull()
    act(() => {
      toast({ message: 'Saved', duration: 60_000 })
    })
    act(() => vi.advanceTimersByTime(ANNOUNCE_MS))
    expect(polite().textContent).toBe('')
    expect(shown().getByText('Saved')).toBeTruthy()
  })

  test('a danger toast is said at once, in the assertive region', () => {
    render(<Toaster />)
    act(() => {
      toast({ message: 'The app could not be moved', tone: 'danger' })
    })
    expect(assertive().textContent).toBe('The app could not be moved')
    expect(polite().textContent).toBe('')
  })

  test('leaves by itself, not while the pointer is on it', () => {
    render(<Toaster />)
    act(() => {
      toast({ message: 'Saved', duration: 1000 })
    })
    const el = shown().getByText('Saved').closest('.ui-toast') as HTMLElement
    fireEvent.pointerEnter(el)
    act(() => vi.advanceTimersByTime(5000))
    expect(shown().queryByText('Saved')).not.toBeNull()
    fireEvent.pointerLeave(el)
    act(() => vi.advanceTimersByTime(999))
    expect(shown().queryByText('Saved')).not.toBeNull()
    act(() => vi.advanceTimersByTime(1))
    expect(shown().queryByText('Saved')).toBeNull()
  })

  test('stays while the focus is on it, though the pointer has left', () => {
    render(<Toaster />)
    act(() => {
      toast({ message: 'Card archived', duration: 1000, action: { label: 'Undo', onClick: () => {} } })
    })
    const el = shown().getByText('Card archived').closest('.ui-toast') as HTMLElement
    fireEvent.pointerEnter(el)
    fireEvent.focus(shown().getByRole('button', { name: 'Undo' }))
    fireEvent.pointerLeave(el)
    act(() => vi.advanceTimersByTime(5000))
    expect(shown().queryByText('Card archived')).not.toBeNull()
    fireEvent.blur(shown().getByRole('button', { name: 'Undo' }), { relatedTarget: document.body })
    act(() => vi.advanceTimersByTime(1000))
    expect(shown().queryByText('Card archived')).toBeNull()
  })

  test('can be dismissed, by its button and by its id', () => {
    render(<Toaster />)
    let id = 0
    act(() => {
      toast('One')
      id = toast('Two')
    })
    fireEvent.click(shown().getAllByRole('button', { name: 'Dismiss' })[0])
    expect(shown().queryByText('One')).toBeNull()
    act(() => dismissToast(id))
    expect(shown().queryByText('Two')).toBeNull()
  })

  test('pressed from the keyboard, it leaves the focus beside it, not on the page', () => {
    render(
      <>
        <button type="button">Archive</button>
        <Toaster />
      </>,
    )
    const archive = document.querySelector('button') as HTMLButtonElement
    act(() => {
      toast({ message: 'Card archived', action: { label: 'Undo', onClick: () => {} } })
    })
    // From the page into the toast, then its action.
    act(() => archive.focus())
    const undo = shown().getByRole('button', { name: 'Undo' })
    act(() => undo.focus())
    act(() => undo.click())
    expect(shown().queryByText('Card archived')).toBeNull()
    expect(document.activeElement).toBe(archive)

    // With two toasts, dismissing one goes to the other.
    act(() => {
      toast('First')
      toast('Second')
    })
    const dismiss = shown().getAllByRole('button', { name: 'Dismiss' })
    act(() => dismiss[1].focus())
    act(() => dismiss[1].click())
    expect(shown().queryByText('Second')).toBeNull()
    expect(stack().contains(document.activeElement)).toBe(true)
    expect(shown().getByText('First').closest('.ui-toast')?.contains(document.activeElement)).toBe(true)
  })

  test('an action that moves the focus itself keeps that', () => {
    render(
      <>
        <button type="button">Archive</button>
        <input aria-label="Title" />
        <Toaster />
      </>,
    )
    const title = document.querySelector('input') as HTMLInputElement
    act(() => {
      toast({ message: 'Card archived', action: { label: 'Undo', onClick: () => title.focus() } })
    })
    const undo = shown().getByRole('button', { name: 'Undo' })
    act(() => (document.querySelector('button') as HTMLButtonElement).focus())
    act(() => undo.focus())
    act(() => undo.click())
    expect(document.activeElement).toBe(title)
  })

  test('stacks, newest last, and keeps the latest four', () => {
    render(<Toaster />)
    act(() => {
      for (const n of [1, 2, 3, 4, 5]) toast(`Toast ${n}`)
    })
    expect([...stack().querySelectorAll('.ui-toast-message')].map((e) => e.textContent)).toEqual([
      'Toast 2',
      'Toast 3',
      'Toast 4',
      'Toast 5',
    ])
  })

  test('an action runs and dismisses the toast', () => {
    render(<Toaster />)
    const undo = vi.fn()
    act(() => {
      toast({ message: 'Card archived', action: { label: 'Undo', onClick: undo } })
    })
    fireEvent.click(shown().getByRole('button', { name: 'Undo' }))
    expect(undo).toHaveBeenCalledOnce()
    expect(shown().queryByText('Card archived')).toBeNull()
  })

  test('moves into a modal dialog while it is open, on top, without saying anything again', () => {
    render(<Toaster />)
    act(() => {
      toast({ message: 'Saved', duration: 60_000 })
    })
    act(() => vi.advanceTimersByTime(ANNOUNCE_MS))
    const show = vi.spyOn(stack(), 'showPopover')
    const { unmount } = render(<Dialog open onClose={() => {}} title="Move the card" />)
    expect(stack().closest('dialog')).toBe(document.querySelector('dialog'))
    expect(polite().closest('dialog')).toBe(document.querySelector('dialog'))
    expect(show).toHaveBeenCalled()
    expect(polite().textContent).toBe('')
    unmount()
    expect(stack().closest('dialog')).toBeNull()
    expect(stack().isConnected).toBe(true)
    expect(stack().hasAttribute('data-popover-open')).toBe(true)
    expect(polite().textContent).toBe('')
    expect(shown().getByText('Saved')).toBeTruthy()
  })

  test('a second Toaster draws and says nothing, and takes over when the first goes', () => {
    const first = render(<Toaster />)
    render(<Toaster />)
    expect(document.querySelectorAll('.ui-toaster')).toHaveLength(1)
    expect(document.querySelectorAll('[aria-live="polite"]')).toHaveLength(1)
    act(() => {
      toast('Once')
    })
    expect(document.querySelectorAll('.ui-toaster-said')).toHaveLength(1)
    first.unmount()
    expect(document.querySelectorAll('.ui-toaster')).toHaveLength(1)
    expect(shown().getByText('Once')).toBeTruthy()
  })

  test('toast() draws the stack itself when the app hasn’t', async () => {
    await act(async () => {
      toast('Link copied')
    })
    await act(async () => {
      await vi.runAllTimersAsync()
    })
    expect(stack()).not.toBeNull()
  })
})

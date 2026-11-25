// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import { Dialog } from './Dialog'
import { dismissToast, resetToasts, Toaster, toast } from './Toast'

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

describe('Toast', () => {
  test('the live regions are there, empty, before a toast goes in', () => {
    render(<Toaster />)
    expect(polite().children).toHaveLength(0)
    expect(assertive().children).toHaveLength(0)
    act(() => {
      toast('Link copied')
    })
    expect(polite().textContent).toContain('Link copied')
    expect(stack().hasAttribute('data-popover-open')).toBe(true)
  })

  test('a danger toast is said at once, in the assertive region', () => {
    render(<Toaster />)
    act(() => {
      toast({ message: 'The app could not be moved', tone: 'danger' })
    })
    expect(assertive().textContent).toContain('The app could not be moved')
    expect(polite().textContent).toBe('')
  })

  test('leaves by itself, not while the pointer is on it', () => {
    render(<Toaster />)
    act(() => {
      toast({ message: 'Saved', duration: 1000 })
    })
    const el = screen.getByText('Saved').closest('.ui-toast') as HTMLElement
    fireEvent.pointerEnter(el)
    act(() => vi.advanceTimersByTime(5000))
    expect(screen.queryByText('Saved')).not.toBeNull()
    fireEvent.pointerLeave(el)
    act(() => vi.advanceTimersByTime(999))
    expect(screen.queryByText('Saved')).not.toBeNull()
    act(() => vi.advanceTimersByTime(1))
    expect(screen.queryByText('Saved')).toBeNull()
  })

  test('can be dismissed, by its button and by its id', () => {
    render(<Toaster />)
    let id = 0
    act(() => {
      toast('One')
      id = toast('Two')
    })
    fireEvent.click(screen.getAllByRole('button', { name: 'Dismiss' })[0])
    expect(screen.queryByText('One')).toBeNull()
    act(() => dismissToast(id))
    expect(screen.queryByText('Two')).toBeNull()
  })

  test('stacks, newest last, and keeps the latest four', () => {
    render(<Toaster />)
    act(() => {
      for (const n of [1, 2, 3, 4, 5]) toast(`Toast ${n}`)
    })
    expect([...polite().querySelectorAll('.ui-toast')].map((e) => e.textContent?.match(/Toast \d/)?.[0])).toEqual([
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
    fireEvent.click(screen.getByRole('button', { name: 'Undo' }))
    expect(undo).toHaveBeenCalledOnce()
    expect(screen.queryByText('Card archived')).toBeNull()
  })

  test('moves into a modal dialog while it is open (outside it the page is inert), on top', () => {
    render(<Toaster />)
    const show = vi.spyOn(stack(), 'showPopover')
    const { unmount } = render(<Dialog open onClose={() => {}} title="Move the card" />)
    expect(stack().closest('dialog')).toBe(document.querySelector('dialog'))
    expect(show).toHaveBeenCalled()
    unmount()
    expect(stack().closest('dialog')).toBeNull()
    expect(stack().isConnected).toBe(true)
    expect(stack().hasAttribute('data-popover-open')).toBe(true)
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

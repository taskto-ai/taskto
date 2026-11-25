// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { StrictMode, useState } from 'react'
import { afterEach, describe, expect, test, vi } from 'vitest'
import { Button } from './Button'
import { ConfirmDialog, useConfirm } from './ConfirmDialog'
import { Dialog } from './Dialog'
import { modalOpen } from './layers'

afterEach(cleanup)

const dialog = () => document.querySelector('dialog') as HTMLDialogElement

function Harness({ onClose }: { onClose?: () => void }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Rename
      </button>
      <Dialog
        open={open}
        onClose={() => {
          onClose?.()
          setOpen(false)
        }}
        title="Rename the app"
        description="The new name shows in the menu."
        actions={<Button>Save</Button>}
      >
        <input aria-label="Name" />
      </Dialog>
    </>
  )
}

function open() {
  const opener = screen.getByRole('button', { name: 'Rename' })
  opener.focus()
  fireEvent.click(opener)
  return opener
}

describe('Dialog', () => {
  test('is not there while closed', () => {
    render(<Harness />)
    expect(dialog()).toBeNull()
  })

  test('opens modal, named by its title and described by its line', () => {
    render(<Harness />)
    open()
    expect(dialog().open).toBe(true)
    // showModal, not show: the polyfill marks a modal one.
    expect(dialog().dataset.modal).toBe('')
    expect(screen.getByRole('dialog', { name: 'Rename the app' })).toBe(dialog())
    expect(dialog().getAttribute('aria-describedby')).toBe(
      screen.getByText('The new name shows in the menu.').getAttribute('id'),
    )
    expect(modalOpen()).toBe(true)
  })

  test('its top is not a banner of the page', () => {
    render(<Harness />)
    open()
    expect(screen.queryByRole('banner')).toBeNull()
  })

  test('puts the focus on the first thing in its body, not on the close button', () => {
    render(<Harness />)
    open()
    expect(document.activeElement).toBe(screen.getByRole('textbox', { name: 'Name' }))
  })

  test('Escape asks the screen to close it, which gives the focus back', () => {
    const onClose = vi.fn()
    render(<Harness onClose={onClose} />)
    const opener = open()
    const cancel = new Event('cancel', { cancelable: true })
    act(() => {
      dialog().dispatchEvent(cancel)
    })
    expect(cancel.defaultPrevented).toBe(true)
    expect(onClose).toHaveBeenCalledOnce()
    expect(dialog()).toBeNull()
    expect(document.activeElement).toBe(opener)
    expect(modalOpen()).toBe(false)
  })

  test('the close button closes it, and is named in the language', () => {
    const onClose = vi.fn()
    render(<Harness onClose={onClose} />)
    open()
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(onClose).toHaveBeenCalledOnce()
    expect(dialog()).toBeNull()
  })

  test('when the browser closes it by itself, the screen hears of it', async () => {
    const onClose = vi.fn()
    render(<Harness onClose={onClose} />)
    open()
    await act(async () => {
      dialog().close()
      await new Promise((r) => setTimeout(r))
    })
    expect(onClose).toHaveBeenCalledOnce()
  })

  test('one that is not dismissible has no close button, and neither Escape nor the browser closes it', async () => {
    const onClose = vi.fn()
    render(
      <Dialog
        open
        onClose={onClose}
        title="The account changed"
        role="alertdialog"
        dismissible={false}
        actions={
          <>
            <Button variant="danger">Discard</Button>
            <Button data-autofocus>Reload this page</Button>
          </>
        }
      />,
    )
    expect(screen.queryByRole('button', { name: 'Close' })).toBeNull()
    const cancel = new Event('cancel', { cancelable: true })
    act(() => {
      dialog().dispatchEvent(cancel)
    })
    expect(cancel.defaultPrevented).toBe(true)
    // A real browser that reopens a dialog focuses the first button in it (here the destructive
    // one); jsdom moves no focus, so the test puts it there.
    screen.getByRole('button', { name: 'Discard' }).focus()
    await act(async () => {
      dialog().close()
      await new Promise((r) => setTimeout(r))
    })
    expect(onClose).not.toHaveBeenCalled()
    expect(dialog().open).toBe(true)
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Reload this page' }))
    expect(screen.getByRole('alertdialog', { name: 'The account changed' })).toBeTruthy()
  })
})

function ConfirmScreen({ answers }: { answers: boolean[] }) {
  const [confirmDialog, confirm] = useConfirm()
  return (
    <>
      <button
        type="button"
        onClick={async () => {
          answers.push(await confirm({ title: 'Remove Carla?', confirmLabel: 'Remove', tone: 'danger' }))
        }}
      >
        Remove
      </button>
      {confirmDialog}
    </>
  )
}

test('stays open under StrictMode, whose second mount follows a close', async () => {
  const onClose = vi.fn()
  render(
    <StrictMode>
      <Harness onClose={onClose} />
    </StrictMode>,
  )
  open()
  // The close event of the first mount's cleanup arrives in a later task.
  await act(async () => {
    await new Promise((r) => setTimeout(r))
  })
  expect(onClose).not.toHaveBeenCalled()
  expect(dialog()?.open).toBe(true)
  expect(modalOpen()).toBe(true)
})

describe('ConfirmDialog', () => {
  test('a question with Cancel focused first and the action named, in the danger tone', () => {
    const onConfirm = vi.fn()
    const onCancel = vi.fn()
    render(
      <ConfirmDialog
        open
        title="Delete Equipment?"
        confirmLabel="Delete the app"
        tone="danger"
        onConfirm={onConfirm}
        onCancel={onCancel}
      >
        Its records go with it. This can't be undone.
      </ConfirmDialog>,
    )
    const box = screen.getByRole('alertdialog', { name: 'Delete Equipment?' })
    expect(box.textContent).toContain("This can't be undone.")
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }))
    const action = screen.getByRole('button', { name: 'Delete the app' })
    expect(action.className).toContain('ui-button-danger')
    fireEvent.click(action)
    expect(onConfirm).toHaveBeenCalledOnce()
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(onCancel).toHaveBeenCalledOnce()
  })

  test('while busy, nothing closes it', () => {
    const onCancel = vi.fn()
    render(<ConfirmDialog open busy title="Delete?" confirmLabel="Delete" onConfirm={() => {}} onCancel={onCancel} />)
    act(() => {
      dialog().dispatchEvent(new Event('cancel', { cancelable: true }))
    })
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(onCancel).not.toHaveBeenCalled()
  })

  test('useConfirm asks from an event handler and answers', async () => {
    const answers: boolean[] = []
    render(<ConfirmScreen answers={answers} />)
    fireEvent.click(screen.getByRole('button', { name: 'Remove' }))
    await act(async () => {
      fireEvent.click(screen.getAllByRole('button', { name: 'Remove' })[1])
    })
    fireEvent.click(screen.getByRole('button', { name: 'Remove' }))
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    })
    expect(answers).toEqual([true, false])
    expect(dialog()).toBeNull()
  })
})

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

// A screen that keeps its confirmation open while the action runs, and to show what went wrong.
function BusyScreen() {
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Delete
      </button>
      <ConfirmDialog
        open={open}
        busy={busy}
        title="Delete Equipment?"
        confirmLabel="Delete the app"
        tone="danger"
        onConfirm={() => setBusy(true)}
        onCancel={() => setOpen(false)}
      >
        {busy ? 'Deleting…' : 'Its records go with it.'}
      </ConfirmDialog>
      <button type="button" onClick={() => setBusy(false)}>
        Fail
      </button>
    </>
  )
}

const pressEscape = () => fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' })
const nextTasks = () =>
  act(async () => {
    await new Promise((r) => setTimeout(r))
    await new Promise((r) => setTimeout(r))
  })

describe('a dialog the browser closes while the screen keeps it open', () => {
  test('opens again with the focus inside, and stays the topmost modal', async () => {
    render(<BusyScreen />)
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
    fireEvent.click(screen.getByRole('button', { name: 'Delete the app' }))
    // The first Escape the page may refuse; the next ones without a press in between it can't.
    for (let i = 0; i < 3; i++) {
      pressEscape()
      await nextTasks()
    }
    expect(dialog().open).toBe(true)
    expect(dialog().contains(document.activeElement)).toBe(true)
    expect(modalOpen()).toBe(true)
    expect(screen.getByRole('alertdialog', { name: 'Delete Equipment?' }).textContent).toContain('Deleting…')
  })

  test('when the screen lets it go, it goes, and no modal is left behind', async () => {
    render(<BusyScreen />)
    const opener = screen.getByRole('button', { name: 'Delete' })
    opener.focus()
    fireEvent.click(opener)
    pressEscape()
    await nextTasks()
    expect(dialog()).toBeNull()
    expect(modalOpen()).toBe(false)
    expect(document.activeElement).toBe(opener)
  })

  test('one that is not dismissible opens again at once', async () => {
    const onClose = vi.fn()
    render(
      <Dialog open onClose={onClose} title="The account changed" role="alertdialog" dismissible={false}>
        <Button>Reload this page</Button>
      </Dialog>,
    )
    expect(screen.queryByRole('button', { name: 'Close' })).toBeNull()
    pressEscape()
    pressEscape()
    await act(async () => {
      await new Promise((r) => setTimeout(r))
    })
    expect(onClose).not.toHaveBeenCalled()
    expect(dialog().open).toBe(true)
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Reload this page' }))
  })

  test('one that is not dismissible, reopened by the browser, gives the focus back to its chosen button', async () => {
    render(
      <Dialog
        open
        onClose={() => {}}
        title="Records of another account"
        role="alertdialog"
        dismissible={false}
        actions={
          <>
            <Button variant="danger">Discard them</Button>
            <Button data-autofocus>Sign out</Button>
          </>
        }
      />,
    )
    // A real browser that reopens a dialog focuses its first button, here the destructive one;
    // jsdom moves no focus, so the test puts it there.
    screen.getByRole('button', { name: 'Discard them' }).focus()
    await act(async () => {
      dialog().close()
      await new Promise((r) => setTimeout(r))
    })
    expect(dialog().open).toBe(true)
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Sign out' }))
  })

  test('its top is not a banner of the page', () => {
    render(<Dialog open onClose={() => {}} title="Rename the app" />)
    expect(screen.queryByRole('banner')).toBeNull()
  })

  test('a press outside closes it only when asked to', () => {
    const onClose = vi.fn()
    const { rerender } = render(<Dialog open onClose={onClose} title="Move the card" />)
    fireEvent.click(dialog(), { clientX: -10, clientY: -10 })
    expect(onClose).not.toHaveBeenCalled()
    rerender(<Dialog open onClose={onClose} title="Move the card" closeOnOutsidePress />)
    fireEvent.click(dialog(), { clientX: -10, clientY: -10 })
    expect(onClose).toHaveBeenCalledOnce()
  })
})

// A screen that asks, and says what it was told.
function Asker({ onAnswer, ask }: { onAnswer: (yes: boolean) => void; ask: number }) {
  const [confirmDialog, confirm] = useConfirm()
  return (
    <>
      <button type="button" onClick={async () => onAnswer(await confirm({ title: `Q${ask}`, confirmLabel: 'Yes' }))}>
        Ask
      </button>
      {confirmDialog}
    </>
  )
}
describe('useConfirm', () => {
  test('a question left open when the component goes is a no', async () => {
    const answers: boolean[] = []
    const { unmount } = render(<Asker ask={1} onAnswer={(a) => answers.push(a)} />)
    fireEvent.click(screen.getByRole('button', { name: 'Ask' }))
    expect(dialog()).not.toBeNull()
    await act(async () => {
      unmount()
    })
    expect(answers).toEqual([false])
  })

  test('a second question settles the first as a no', async () => {
    const answers: boolean[] = []
    render(<Asker ask={1} onAnswer={(a) => answers.push(a)} />)
    fireEvent.click(screen.getByRole('button', { name: 'Ask' }))
    await act(async () => {
      fireEvent.click(screen.getAllByRole('button', { name: 'Ask' })[0])
    })
    expect(answers).toEqual([false])
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Yes' }))
    })
    expect(answers).toEqual([false, true])
  })
})

// A screen that asks one question, then another before the first is answered.
function TwoQuestions({ answers }: { answers: boolean[] }) {
  const [confirmDialog, confirm] = useConfirm()
  return (
    <>
      <button
        type="button"
        onClick={async () => answers.push(await confirm({ title: 'Delete Equipment?', confirmLabel: 'Delete' }))}
      >
        First
      </button>
      <button
        type="button"
        onClick={async () => answers.push(await confirm({ title: 'Remove Carla?', confirmLabel: 'Remove' }))}
      >
        Second
      </button>
      {confirmDialog}
    </>
  )
}

describe('the focus when a dialog opens', () => {
  test('passes over what can’t take it (hidden, or a disabled marked one) to the next', () => {
    render(
      <Dialog open onClose={() => {}} title="Rename">
        <button type="button" data-autofocus disabled>
          Busy
        </button>
        <input hidden aria-label="Old name" />
        <div hidden>
          <input aria-label="Older name" />
        </div>
        <input aria-label="Name" />
      </Dialog>,
    )
    expect(document.activeElement).toBe(screen.getByRole('textbox', { name: 'Name' }))
  })

  test('a question that replaces another starts again on Cancel', async () => {
    const answers: boolean[] = []
    render(<TwoQuestions answers={answers} />)
    fireEvent.click(screen.getByRole('button', { name: 'First' }))
    screen.getByRole('button', { name: 'Delete' }).focus()
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Second' }))
    })
    expect(answers).toEqual([false])
    expect(screen.getByRole('alertdialog', { name: 'Remove Carla?' })).toBeTruthy()
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }))
  })
})

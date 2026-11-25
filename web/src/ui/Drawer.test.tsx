// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, test } from 'vitest'
import { Dialog } from './Dialog'
import { Drawer } from './Drawer'
import { MenuItem, MoreMenu } from './Menu'

afterEach(cleanup)

function Board({ dialog = false }: { dialog?: boolean }) {
  const [open, setOpen] = useState(false)
  return (
    <div style={{ overflow: 'hidden' }} data-testid="board">
      <button type="button" onClick={() => setOpen(true)}>
        Write the launch post
      </button>
      <button type="button">Another card</button>
      <Drawer
        open={open}
        onClose={() => setOpen(false)}
        title="Write the launch post"
        headerActions={
          <MoreMenu>
            <MenuItem onSelect={() => {}}>Archive</MenuItem>
          </MoreMenu>
        }
        footer={<button type="button">Comment</button>}
      >
        <p>Draft the post for the launch.</p>
        <Dialog open={dialog} onClose={() => {}} title="Move the card" />
      </Drawer>
    </div>
  )
}

const region = () => screen.queryByRole('region', { name: 'Write the launch post' })

function open() {
  const card = screen.getByRole('button', { name: 'Write the launch post' })
  card.focus()
  fireEvent.click(card)
  return card
}

describe('Drawer', () => {
  test('a region named by its title, drawn into <body> (out of any clipping box), with the focus in it', () => {
    render(<Board />)
    expect(region()).toBeNull()
    open()
    const drawer = region() as HTMLElement
    expect(drawer).not.toBeNull()
    expect(drawer.parentElement).toBe(document.body)
    expect(screen.getByTestId('board').contains(drawer)).toBe(false)
    expect(document.activeElement).toBe(drawer)
    expect(screen.getByRole('heading', { level: 2, name: 'Write the launch post' })).toBeTruthy()
  })

  test('is not modal: the page stays usable', () => {
    render(<Board />)
    open()
    const other = screen.getByRole('button', { name: 'Another card' })
    expect(other.closest('[inert]')).toBeNull()
    other.focus()
    expect(document.activeElement).toBe(other)
    expect(region()).not.toBeNull()
  })

  test('Escape closes it and gives the focus back to the card that opened it', () => {
    render(<Board />)
    const card = open()
    fireEvent.keyDown(region() as HTMLElement, { key: 'Escape' })
    expect(region()).toBeNull()
    expect(document.activeElement).toBe(card)
  })

  test('the close button closes it', () => {
    render(<Board />)
    open()
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(region()).toBeNull()
  })

  test('Escape in a menu in it closes the menu only', () => {
    render(<Board />)
    open()
    fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
    fireEvent.keyDown(screen.getByRole('menuitem', { name: 'Archive' }), { key: 'Escape' })
    expect(screen.queryByRole('menuitem')).toBeNull()
    expect(region()).not.toBeNull()
  })

  test('Escape leaves it open while a dialog is open over it', () => {
    const { rerender } = render(<Board />)
    open()
    rerender(<Board dialog />)
    fireEvent.keyDown(document.activeElement as Element, { key: 'Escape' })
    expect(region()).not.toBeNull()
  })

  test('a person who went on in the page keeps their place when it closes', () => {
    render(<Board />)
    open()
    const other = screen.getByRole('button', { name: 'Another card' })
    other.focus()
    fireEvent.keyDown(other, { key: 'Escape' })
    expect(region()).toBeNull()
    expect(document.activeElement).toBe(other)
  })
})

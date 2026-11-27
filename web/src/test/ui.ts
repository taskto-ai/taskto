import { fireEvent, screen, within } from '@testing-library/react'

export function chooseFromMenu(menu: string, item: string) {
  fireEvent.click(screen.getByRole('button', { name: menu }))
  fireEvent.click(screen.getByRole('menuitem', { name: item }))
}

export async function answer(button: string) {
  const dialog = await screen.findByRole('alertdialog')
  fireEvent.click(within(dialog).getByRole('button', { name: button }))
}

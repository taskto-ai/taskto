// What tests do with the component library's overlays: choose from a "more" menu, answer the
// question of a ConfirmDialog. By role and name, as a person (or a screen reader) finds them.

import { fireEvent, screen, within } from '@testing-library/react'

// Opens the menu behind the button named `menu`, and chooses the item named `item`.
export function chooseFromMenu(menu: string, item: string) {
  fireEvent.click(screen.getByRole('button', { name: menu }))
  fireEvent.click(screen.getByRole('menuitem', { name: item }))
}

// Presses the button named `button` in the confirmation that is open.
export async function answer(button: string) {
  const dialog = await screen.findByRole('alertdialog')
  fireEvent.click(within(dialog).getByRole('button', { name: button }))
}

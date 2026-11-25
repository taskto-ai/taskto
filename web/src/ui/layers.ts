// What the overlays need to know about each other.
//
// A modal Dialog and an open Menu are in the browser's top layer, in the order they opened; a
// Drawer is not (it is beside the page, at --z-drawer), and the toasts are a popover that must
// stay above a dialog and usable. So:
// - a Dialog says when it opens and closes: the Drawer's Escape leaves an open dialog alone, and
//   the toasts move into the topmost modal dialog (everything outside it is inert, a popover in
//   the top layer too) and go back on top;
// - a Menu or a Dialog that handles Escape stops it there (preventDefault), so a Drawer under it
//   doesn't close as well.

const modals: HTMLElement[] = []
const listeners = new Set<() => void>()

function changed() {
  for (const fn of listeners) fn()
}

export function modalOpened(dialog: HTMLElement): void {
  modals.push(dialog)
  changed()
}

export function modalClosed(dialog: HTMLElement): void {
  const i = modals.lastIndexOf(dialog)
  if (i >= 0) modals.splice(i, 1)
  changed()
}

export function modalOpen(): boolean {
  return modals.length > 0
}

// The modal dialog on top, if one is open.
export function topModal(): HTMLElement | null {
  return modals.at(-1) ?? null
}

// Called after a modal dialog opens or closes.
export function onModalChange(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

// The keyboard-focusable elements inside an element, in order.
const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"]), [contenteditable="true"]'

export function focusables(root: Element): HTMLElement[] {
  return [...root.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((el) => !el.closest('[inert]'))
}

// Focus an element that may have gone, or be inert, since it was remembered.
export function refocus(el: Element | null): void {
  if (el instanceof HTMLElement && el.isConnected && el !== document.body) el.focus()
}

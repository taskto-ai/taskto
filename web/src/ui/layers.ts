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

export function modalReopened(dialog: HTMLElement): void {
  const i = modals.lastIndexOf(dialog)
  if (i >= 0) modals.splice(i, 1)
  modals.push(dialog)
  changed()
}

export function modalOpen(): boolean {
  return modals.length > 0
}

export function topModal(): HTMLElement | null {
  return modals.at(-1) ?? null
}

export function onModalChange(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"]), [contenteditable="true"]'

export function focusables(root: Element): HTMLElement[] {
  return [...root.querySelectorAll<HTMLElement>(FOCUSABLE)].filter(
    (el) =>
      !el.closest('[inert], [hidden]') &&
      (typeof el.checkVisibility !== 'function' || el.checkVisibility({ visibilityProperty: true })),
  )
}

export function refocus(el: Element | null): void {
  if (el instanceof HTMLElement && el.isConnected && el !== document.body) el.focus()
}

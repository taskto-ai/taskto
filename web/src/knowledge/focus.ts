export function focusLost(): boolean {
  const at = document.activeElement
  if (!(at instanceof HTMLElement) || at === document.body || !at.isConnected) return true
  if (at.closest('[hidden]')) return true
  return typeof at.checkVisibility === 'function' && !at.checkVisibility()
}

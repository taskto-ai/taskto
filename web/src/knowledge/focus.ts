// Where the keyboard is after a part of the page went away: nowhere (the page itself), on an
// element no longer in the page, or on one hidden now (the list, on a phone, once a document
// opens). The parts that replace it then give the focus a place.
export function focusLost(): boolean {
  const at = document.activeElement
  if (!(at instanceof HTMLElement) || at === document.body || !at.isConnected) return true
  // Inside a part the page hid (the document behind the results), where the browser can't say.
  if (at.closest('[hidden]')) return true
  return typeof at.checkVisibility === 'function' && !at.checkVisibility()
}

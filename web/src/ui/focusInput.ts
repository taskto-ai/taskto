// Whether the focus on an element came from the keyboard, for the overlays that act on it only
// then (a tooltip opens; a toast that goes hands the focus on).
//
// The last input on the page, a key or a pointer, is heard once for the whole page. Before any,
// focus counts as the keyboard's (a page that moves the focus itself, on load, does it for a
// keyboard).
let lastInput: 'keyboard' | 'pointer' = 'keyboard'
let listening = false

export function listenForInput(): void {
  if (listening || typeof document === 'undefined') return
  listening = true
  document.addEventListener('keydown', () => (lastInput = 'keyboard'), true)
  document.addEventListener('pointerdown', () => (lastInput = 'pointer'), true)
}

// The last input was a key, and the browser shows the focus (:focus-visible), where it knows it.
export function focusVisible(el: Element): boolean {
  listenForInput()
  if (lastInput !== 'keyboard') return false
  try {
    return el.matches(':focus-visible')
  } catch {
    return true
  }
}

let lastInput: 'keyboard' | 'pointer' = 'keyboard'
let listening = false

export function listenForInput(): void {
  if (listening || typeof document === 'undefined') return
  listening = true
  document.addEventListener('keydown', () => (lastInput = 'keyboard'), true)
  document.addEventListener('pointerdown', () => (lastInput = 'pointer'), true)
}

export function focusVisible(el: Element): boolean {
  listenForInput()
  if (lastInput !== 'keyboard') return false
  try {
    return el.matches(':focus-visible')
  } catch {
    return true
  }
}

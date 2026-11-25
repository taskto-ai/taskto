// Keeps the layout above the on-screen keyboard where the browser doesn't (docs/pwa.md).
//
// Chrome on Android shrinks the page with the keyboard (interactive-widget=resizes-content in
// index.html), so nothing is needed there. Safari on iOS leaves the layout viewport alone and
// scrolls the page to the field, which can push the top bar off screen and leave the composer
// under the keyboard. There the visual viewport is the part left visible: while it is much
// shorter than the window, the app is sized to it and moved to its top (html[data-keyboard],
// ui/base.css). Pinch-zoom shrinks it too, and isn't a keyboard.

const KEYBOARD_MIN_PX = 120

export function trackKeyboard(win: Window = window): () => void {
  const vv = win.visualViewport
  if (!vv) return () => {}
  const root = win.document.documentElement
  const update = () => {
    const hidden = win.innerHeight - vv.height - vv.offsetTop
    const open = vv.scale <= 1.01 && win.innerHeight - vv.height > KEYBOARD_MIN_PX && hidden >= 0
    if (open) {
      root.dataset.keyboard = 'open'
      root.style.setProperty('--tt-viewport-h', `${vv.height}px`)
      root.style.setProperty('--tt-viewport-top', `${vv.offsetTop}px`)
    } else {
      delete root.dataset.keyboard
      root.style.removeProperty('--tt-viewport-h')
      root.style.removeProperty('--tt-viewport-top')
    }
  }
  vv.addEventListener('resize', update)
  vv.addEventListener('scroll', update)
  update()
  return () => {
    vv.removeEventListener('resize', update)
    vv.removeEventListener('scroll', update)
  }
}

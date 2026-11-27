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

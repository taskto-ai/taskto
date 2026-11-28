import { act, configure } from '@testing-library/react'
import { afterEach, beforeEach } from 'vitest'
import { resetSession } from '../session'
import { closeQueued, resetCloses } from './dialogEvents'

configure({ asyncUtilTimeout: 4000 })

afterEach(() => {
  try {
    globalThis.localStorage?.clear()
  } catch {}
})

if (typeof HTMLDialogElement !== 'undefined' && !HTMLDialogElement.prototype.showModal) {
  const modals: HTMLDialogElement[] = []
  let refusedSinceActivation = false
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
    if (this.open) throw new DOMException('The dialog is already open.', 'InvalidStateError')
    this.setAttribute('open', '')
    this.dataset.modal = ''
    modals.push(this)
  }
  HTMLDialogElement.prototype.show = function (this: HTMLDialogElement) {
    this.setAttribute('open', '')
  }
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement, value?: string) {
    if (!this.open) return
    this.removeAttribute('open')
    delete this.dataset.modal
    const at = modals.lastIndexOf(this)
    if (at >= 0) modals.splice(at, 1)
    if (value !== undefined) this.returnValue = value
    if (this.contains(document.activeElement)) (document.activeElement as HTMLElement).blur()
    const delivered = closeQueued()
    setTimeout(() => {
      try {
        this.dispatchEvent(new Event('close'))
      } finally {
        delivered()
      }
    })
  }
  const activate = () => {
    refusedSinceActivation = false
  }
  for (const type of ['pointerdown', 'mousedown', 'click']) window.addEventListener(type, activate, true)
  afterEach(() => {
    refusedSinceActivation = false
    modals.length = 0
    resetCloses()
  })
  window.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return activate()
    const top = modals.filter((d) => d.isConnected && d.open).at(-1)
    if (!top || e.defaultPrevented) return
    const cancel = new Event('cancel', { cancelable: !refusedSinceActivation })
    top.dispatchEvent(cancel)
    if (cancel.defaultPrevented) refusedSinceActivation = true
    else top.close()
  })
}
if (typeof HTMLElement !== 'undefined' && !HTMLElement.prototype.showPopover) {
  const open = new WeakSet<HTMLElement>()
  HTMLElement.prototype.showPopover = function (this: HTMLElement) {
    if (open.has(this)) throw new DOMException('The popover is already open.', 'InvalidStateError')
    open.add(this)
    this.dataset.popoverOpen = ''
  }
  HTMLElement.prototype.hidePopover = function (this: HTMLElement) {
    open.delete(this)
    delete this.dataset.popoverOpen
  }
  const style = document.createElement('style')
  style.textContent = '[popover][data-popover-open] { display: block; }'
  document.head.append(style)
  HTMLElement.prototype.togglePopover = function (this: HTMLElement, force?: boolean) {
    const show = force ?? !open.has(this)
    if (show && !open.has(this)) this.showPopover()
    else if (!show) this.hidePopover()
    return show
  }
}

beforeEach(() => resetSession())
afterEach(() => {
  act(() => resetSession())
})

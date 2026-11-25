// Runs before every test file (vite.config.ts, test.setupFiles).

import { configure } from '@testing-library/react'
import { afterEach } from 'vitest'

// How long findBy… and waitFor wait for what the page ends up showing. Testing Library's
// default, 1 s, is tight on a CI runner shared with other jobs: there, a language switch that
// takes about 20 ms on an idle machine (the whole app drawn again in the new language) took
// just over 1 s once, and the test failed with the page about to be right. The tests check
// what the page shows, not how fast it gets there. 4 s stays under the 10 s a whole test has
// (vite.config.ts), so a page that never gets there still fails with Testing Library's own
// message and the page it saw.
configure({ asyncUtilTimeout: 4000 })

// What a page keeps in the browser (an unsent message, the app being built) belongs to the test
// that made it, not the next one.
afterEach(() => {
  try {
    globalThis.localStorage?.clear()
  } catch {
    // A test environment without storage has none to clear.
  }
})

// jsdom has <dialog> and the popover attribute but not their methods. The least of them, so the
// overlays' own logic (src/ui/) can be tested: what the browser does beyond this (the top layer,
// making the page inert, light dismiss) is not imitated, and is checked in a real browser.
if (typeof HTMLDialogElement !== 'undefined' && !HTMLDialogElement.prototype.showModal) {
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
    if (this.open) throw new DOMException('The dialog is already open.', 'InvalidStateError')
    this.setAttribute('open', '')
    this.dataset.modal = ''
  }
  HTMLDialogElement.prototype.show = function (this: HTMLDialogElement) {
    this.setAttribute('open', '')
  }
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement, value?: string) {
    if (!this.open) return
    this.removeAttribute('open')
    delete this.dataset.modal
    if (value !== undefined) this.returnValue = value
    // The browser fires close in a task of its own.
    setTimeout(() => this.dispatchEvent(new Event('close')))
  }
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
  // jsdom hides every [popover] and has no :popover-open to show an open one.
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

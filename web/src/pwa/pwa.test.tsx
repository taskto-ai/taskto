// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { InstallPanel } from './InstallPanel'
import { platformOf, resetInstallInfo } from './install'
import { UpdateBanner } from './UpdateBanner'
import { type Registration, resetUpdateState, track } from './update'
import { trackKeyboard } from './viewport'

beforeEach(() => {
  resetUpdateState()
  resetInstallInfo()
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  delete document.documentElement.dataset.keyboard
})

// A worker and a registration the test drives.
function fakeWorker(state: ServiceWorker['state'] = 'installing') {
  const handlers: Record<string, () => void> = {}
  const worker = {
    state,
    postMessage: vi.fn(),
    addEventListener: (type: string, h: () => void) => {
      handlers[type] = h
    },
  }
  return { worker: worker as unknown as ServiceWorker & { postMessage: ReturnType<typeof vi.fn> }, handlers }
}

function fakeRegistration(waiting: ServiceWorker | null = null) {
  const handlers: Record<string, () => void> = {}
  const reg = {
    waiting,
    installing: null as ServiceWorker | null,
    update: vi.fn(async () => {}),
    addEventListener: (type: string, h: () => void) => {
      handlers[type] = h
    },
  }
  return { reg: reg as Registration & { installing: ServiceWorker | null }, handlers }
}

function fakeContainer(controller: ServiceWorker | null) {
  const handlers: Record<string, () => void> = {}
  return {
    container: {
      controller,
      addEventListener: (type: 'controllerchange', h: () => void) => {
        handlers[type] = h
      },
    },
    handlers,
  }
}

test('an update that installs while the app runs prompts, and reloads nothing by itself', () => {
  const reload = vi.fn()
  const { reg, handlers } = fakeRegistration()
  const { container } = fakeContainer({} as ServiceWorker)
  track(reg, container, reload)
  render(<UpdateBanner />)
  expect(screen.queryByRole('status')).toBeNull()

  const next = fakeWorker()
  act(() => {
    reg.installing = next.worker
    handlers.updatefound()
  })
  expect(screen.queryByRole('status')).toBeNull() // still installing
  act(() => {
    ;(next.worker as { state: string }).state = 'installed'
    next.handlers.statechange()
  })
  expect(screen.getByRole('status').textContent).toContain('A new version of taskto.ai is ready')
  expect(next.worker.postMessage).not.toHaveBeenCalled()
  expect(reload).not.toHaveBeenCalled()
})

test('the first install is not an update: no prompt without a controller', () => {
  const { reg, handlers } = fakeRegistration()
  const { container } = fakeContainer(null)
  track(reg, container, vi.fn())
  const first = fakeWorker()
  act(() => {
    reg.installing = first.worker
    handlers.updatefound()
    ;(first.worker as { state: string }).state = 'installed'
    first.handlers.statechange()
  })
  render(<UpdateBanner />)
  expect(screen.queryByRole('status')).toBeNull()
})

test('Reload to update asks the waiting worker, and reloads only once it has taken over', () => {
  const reload = vi.fn()
  const waiting = fakeWorker('installed')
  const { reg } = fakeRegistration(waiting.worker)
  const { container, handlers } = fakeContainer({} as ServiceWorker)
  track(reg, container, reload)
  render(<UpdateBanner />)
  fireEvent.click(screen.getByRole('button', { name: 'Reload to update' }))
  expect(waiting.worker.postMessage).toHaveBeenCalledWith({ type: 'SKIP_WAITING' })
  expect(reload).not.toHaveBeenCalled()
  act(() => handlers.controllerchange())
  expect(reload).toHaveBeenCalledOnce()
  expect(screen.queryByRole('status')).toBeNull()
})

test("another tab's update never reloads this one", () => {
  const reload = vi.fn()
  const waiting = fakeWorker('installed')
  const { reg } = fakeRegistration(waiting.worker)
  const { container, handlers } = fakeContainer({} as ServiceWorker)
  track(reg, container, reload)
  act(() => handlers.controllerchange()) // this tab never asked
  expect(reload).not.toHaveBeenCalled()
})

test('Later hides the prompt without updating', () => {
  const waiting = fakeWorker('installed')
  const { reg } = fakeRegistration(waiting.worker)
  track(reg, fakeContainer({} as ServiceWorker).container, vi.fn())
  render(<UpdateBanner />)
  fireEvent.click(screen.getByRole('button', { name: 'Later' }))
  expect(screen.queryByRole('status')).toBeNull()
  expect(waiting.worker.postMessage).not.toHaveBeenCalled()
})

test('platforms are told apart, an iPad that says it is a Mac included', () => {
  expect(platformOf('Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)', 'iPhone', 5)).toBe('ios')
  expect(platformOf('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)', 'MacIntel', 5)).toBe('ios')
  expect(platformOf('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)', 'MacIntel', 0)).toBe('desktop')
  expect(platformOf('Mozilla/5.0 (Linux; Android 14; Pixel 8)', 'Linux armv81', 5)).toBe('android')
  expect(platformOf('Mozilla/5.0 (X11; Linux x86_64)', 'Linux x86_64', 0)).toBe('desktop')
})

function stubBrowser({
  ua,
  platform = 'Linux',
  secure = true,
  standalone = false,
}: {
  ua: string
  platform?: string
  secure?: boolean
  standalone?: boolean
}) {
  vi.stubGlobal('navigator', { userAgent: ua, platform, maxTouchPoints: 0 })
  vi.stubGlobal('isSecureContext', secure)
  vi.stubGlobal('matchMedia', () => ({ matches: standalone }))
}

test('on iOS, the guidance is Add to Home Screen, with no install button', () => {
  stubBrowser({ ua: 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)', platform: 'iPhone' })
  render(<InstallPanel />)
  expect(screen.getByRole('heading', { name: 'Install the app' })).toBeTruthy()
  expect(screen.getByText(/Add to Home Screen/)).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Install taskto.ai' })).toBeNull()
})

test('on Android without a prompt, the guidance is the browser menu', () => {
  stubBrowser({ ua: 'Mozilla/5.0 (Linux; Android 14; Pixel 8)' })
  render(<InstallPanel />)
  expect(screen.getByText(/Install app/)).toBeTruthy()
})

test('installed: says so, with no steps', () => {
  stubBrowser({ ua: 'Mozilla/5.0 (X11; Linux x86_64)', standalone: true })
  render(<InstallPanel />)
  expect(screen.getByText(/you are using the installed app/)).toBeTruthy()
  expect(screen.queryByRole('list')).toBeNull()
})

test('without HTTPS, says why it cannot be installed', () => {
  stubBrowser({ ua: 'Mozilla/5.0 (X11; Linux x86_64)', secure: false })
  render(<InstallPanel />)
  expect(screen.getByText(/HTTPS/)).toBeTruthy()
})

test('the on-screen keyboard: the app is sized to the visible viewport, and only then', () => {
  const handlers: Record<string, () => void> = {}
  const vv = {
    height: 800,
    offsetTop: 0,
    scale: 1,
    addEventListener: (t: string, h: () => void) => {
      handlers[t] = h
    },
    removeEventListener: vi.fn(),
  }
  const win = { innerHeight: 800, visualViewport: vv, document } as unknown as Window
  const stop = trackKeyboard(win)
  expect(document.documentElement.dataset.keyboard).toBeUndefined()

  Object.assign(vv, { height: 480, offsetTop: 40 }) // the keyboard is up, the page scrolled
  handlers.resize()
  expect(document.documentElement.dataset.keyboard).toBe('open')
  expect(document.documentElement.style.getPropertyValue('--tt-viewport-h')).toBe('480px')
  expect(document.documentElement.style.getPropertyValue('--tt-viewport-top')).toBe('40px')

  Object.assign(vv, { scale: 2 }) // a pinch-zoom is not a keyboard
  handlers.resize()
  expect(document.documentElement.dataset.keyboard).toBeUndefined()

  Object.assign(vv, { scale: 1, height: 790, offsetTop: 0 }) // a small change (the URL bar) neither
  handlers.resize()
  expect(document.documentElement.dataset.keyboard).toBeUndefined()
  stop()
  expect(vv.removeEventListener).toHaveBeenCalled()
})

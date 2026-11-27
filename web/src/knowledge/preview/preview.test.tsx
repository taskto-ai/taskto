// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { EXPECTED_USER_HEADER } from '../../api'
import { beginSession, endSession, resetSession } from '../../session'
import { fakeFetch } from '../../test/fakes'
import { Content } from '../Content'
import { isWord } from '../docs'
import { renderDocx } from './docx'
import { FilePreview, MAX_PREVIEW_BYTES } from './FilePreview'
import { FRAME_SANDBOX, PreviewFrame, webAddress } from './PreviewFrame'

vi.mock('./docx', () => ({ renderDocx: vi.fn() }))

const rendered = vi.mocked(renderDocx)
const RAW = '/api/workspaces/1/spaces/3/docs/contract.docx?raw=true'

let watcher: MutationObserver | null = null

function loadSrcdocInJsdom() {
  const filled = new WeakSet<HTMLIFrameElement>()
  const fill = (frame: HTMLIFrameElement) => {
    if (filled.has(frame)) return
    filled.add(frame)
    const doc = frame.contentDocument
    const html = frame.getAttribute('srcdoc')
    if (!doc || !html) return
    doc.documentElement.innerHTML = new DOMParser().parseFromString(html, 'text/html').documentElement.innerHTML
    frame.dispatchEvent(new Event('load'))
  }
  watcher = new MutationObserver((records) => {
    for (const record of records)
      for (const node of record.addedNodes)
        for (const frame of node instanceof HTMLIFrameElement
          ? [node]
          : ((node as Element).querySelectorAll?.('iframe') ?? []))
          fill(frame)
  })
  watcher.observe(document.body, { childList: true, subtree: true })
}

beforeEach(() => {
  loadSrcdocInJsdom()
  rendered.mockReset()
  rendered.mockImplementation(async (_bytes, into) => {
    into.innerHTML = '<h1>Master agreement</h1><a id="web" href="https://example.com/terms">terms</a>'
  })
})

afterEach(() => {
  resetSession()
  watcher?.disconnect()
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

function frame() {
  return screen.getByTitle('Preview of contract.docx') as HTMLIFrameElement
}

test('a Word file is fetched raw, drawn by the renderer into the frame, and the frame is named', async () => {
  const calls = fakeFetch({ [`GET ${RAW}`]: { body: {} } })
  render(<FilePreview src={RAW} name="contract.docx" size={1200} />)
  await waitFor(() => expect(rendered).toHaveBeenCalledOnce())
  expect(calls.map((c) => c.path)).toEqual([RAW])
  expect(frame().contentDocument?.querySelector('h1')?.textContent).toBe('Master agreement')
  await waitFor(() => expect(frame().hidden).toBe(false))
  expect(rendered.mock.calls[0][1]).toBe(frame().contentDocument?.body)
})

test('an old version is asked for by its commit, with a signal that ends the request', async () => {
  const at = '/api/workspaces/1/spaces/3/docs/contract.docx?at=abc123&raw=true'
  const calls = fakeFetch({ [`GET ${at}`]: { body: {} } })
  render(<FilePreview src={at} name="contract.docx" size={4} />)
  await waitFor(() => expect(rendered).toHaveBeenCalledOnce())
  expect(calls.map((c) => c.path)).toEqual([at])
  expect(calls[0].signal).toBeInstanceOf(AbortSignal)
})

test('a file over the limit shows the notice and fetches nothing', async () => {
  const calls = fakeFetch({})
  render(<FilePreview src={RAW} name="contract.docx" size={MAX_PREVIEW_BYTES + 1} />)
  expect(await screen.findByText(/This file is over 20 MB/)).toBeTruthy()
  expect(calls).toHaveLength(0)
  expect(screen.queryByTitle('Preview of contract.docx')).toBeNull()
  expect(rendered).not.toHaveBeenCalled()
})

test('a renderer error shows the failure notice, and Download stays beside it', async () => {
  rendered.mockRejectedValue(new Error('not a zip'))
  fakeFetch({ [`GET ${RAW}`]: { body: {} } })
  const doc = { path: 'contract.docx', version: 'a'.repeat(40), size: 12, versioned: true }
  render(<Content url="/api/workspaces/1/spaces/3/docs/contract.docx" doc={doc} onLink={() => null} />)
  expect(await screen.findByText("This file can't be previewed; download it to open it.")).toBeTruthy()
  expect(screen.queryByTitle('Preview of contract.docx')).toBeNull()
  const download = screen.getByRole('link', { name: 'Download' })
  expect(download.getAttribute('href')).toBe(RAW)
})

test('a failed fetch shows the failure notice', async () => {
  fakeFetch({})
  render(<FilePreview src={RAW} name="contract.docx" size={12} />)
  expect(await screen.findByText("This file can't be previewed; download it to open it.")).toBeTruthy()
  expect(rendered).not.toHaveBeenCalled()
})

test('switching documents aborts the previous fetch and draws only the new one', async () => {
  const other = '/api/workspaces/1/spaces/3/docs/other.docx?raw=true'
  const calls = fakeFetch({
    [`GET ${RAW}`]: () => new Promise(() => {}),
    [`GET ${other}`]: { body: {} },
  })
  const { rerender } = render(<FilePreview src={RAW} name="contract.docx" size={1} />)
  await waitFor(() => expect(calls).toHaveLength(1))
  const first = calls[0].signal
  expect(first?.aborted).toBe(false)
  rerender(<FilePreview src={other} name="other.docx" size={1} />)
  await waitFor(() => expect(first?.aborted).toBe(true))
  await waitFor(() => expect(rendered).toHaveBeenCalledOnce())
  expect(calls.map((c) => c.path)).toEqual([RAW, other])
  expect(screen.getByTitle('Preview of other.docx')).toBeTruthy()
})

test('a Word file in Content is previewed above the Download link; other files are not', async () => {
  fakeFetch({ [`GET ${RAW}`]: { body: {} } })
  const base = { version: 'a'.repeat(40), size: 12, versioned: true }
  const { rerender } = render(
    <Content
      url="/api/workspaces/1/spaces/3/docs/contract.docx"
      doc={{ ...base, path: 'contract.docx' }}
      onLink={() => null}
    />,
  )
  await waitFor(() => expect(frame()).toBeTruthy())
  expect(screen.getByRole('link', { name: 'Download' })).toBeTruthy()
  rerender(<Content url="/api/workspaces/1/spaces/3/docs/a.zip" doc={{ ...base, path: 'a.zip' }} onLink={() => null} />)
  expect(screen.queryByTitle(/Preview of/)).toBeNull()
})

test('the frame is sandboxed with allow-same-origin only and carries a policy that blocks everything else', async () => {
  const done = vi.fn()
  render(<PreviewFrame title="t" render={async () => {}} onRendered={done} onFailed={() => {}} />)
  const el = screen.getByTitle('t') as HTMLIFrameElement
  expect(el.getAttribute('sandbox')).toBe('allow-same-origin')
  expect(FRAME_SANDBOX).toBe('allow-same-origin')
  await waitFor(() => expect(done).toHaveBeenCalledOnce())
  const policy = el.contentDocument
    ?.querySelector('meta[http-equiv="Content-Security-Policy"]')
    ?.getAttribute('content')
  expect(policy).toBe("default-src 'none'; style-src 'unsafe-inline'; img-src blob: data:; font-src data:")
})

test('a click on a link inside the frame opens only web addresses, without an opener', async () => {
  const open = vi.spyOn(window, 'open').mockReturnValue(null)
  const done = vi.fn()
  render(
    <PreviewFrame
      title="t"
      render={async (body) => {
        body.innerHTML =
          '<a id="js" href="javascript:alert(1)">x</a><a id="web" href="https://example.com/a?b=1"><span id="inner">y</span></a><a id="rel" href="other.docx">z</a><a id="mail" href="mailto:a@b.c">m</a><a id="data" href="data:text/html,hi">d</a>'
      }}
      onRendered={done}
      onFailed={() => {}}
    />,
  )
  await waitFor(() => expect(done).toHaveBeenCalledOnce())
  const doc = (screen.getByTitle('t') as HTMLIFrameElement).contentDocument as Document
  for (const id of ['js', 'rel', 'mail', 'data']) {
    const event = new MouseEvent('click', { bubbles: true, cancelable: true })
    doc.getElementById(id)?.dispatchEvent(event)
    expect(event.defaultPrevented).toBe(true)
  }
  expect(open).not.toHaveBeenCalled()
  const event = new MouseEvent('click', { bubbles: true, cancelable: true })
  doc.getElementById('inner')?.dispatchEvent(event)
  expect(event.defaultPrevented).toBe(true)
  expect(open).toHaveBeenCalledExactlyOnceWith('https://example.com/a?b=1', '_blank', 'noopener,noreferrer')
})

test('the frame renders once and reports a renderer that throws', async () => {
  const failed = vi.fn()
  const done = vi.fn()
  const render1 = vi.fn(async () => {
    throw new Error('bad')
  })
  const { rerender } = render(<PreviewFrame title="t" render={render1} onRendered={done} onFailed={failed} />)
  await waitFor(() => expect(failed).toHaveBeenCalledOnce())
  rerender(<PreviewFrame title="t" render={render1} onRendered={done} onFailed={failed} />)
  expect(render1).toHaveBeenCalledOnce()
  expect(done).not.toHaveBeenCalled()
})

test('only http and https addresses are web addresses', () => {
  expect(webAddress('https://example.com')).toBe('https://example.com/')
  expect(webAddress('http://example.com/x')).toBe('http://example.com/x')
  for (const bad of [
    null,
    '',
    'javascript:alert(1)',
    'data:text/html,x',
    'mailto:a@b.c',
    'file:///etc/passwd',
    '#top',
    'x.docx',
  ])
    expect(webAddress(bad)).toBeNull()
  fireEvent.click(document.body)
})

function heldFetch(urls: string[]) {
  const answers = new Map<string, (res: Response) => void>()
  const requested: string[] = []
  const headers: Record<string, string>[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      requested.push(url)
      headers.push(Object.fromEntries(new Headers(init?.headers).entries()))
      if (!urls.includes(url)) return new Response('{}', { status: 404 })
      return new Promise<Response>((resolve) => answers.set(url, resolve))
    }),
  )
  const bytes = (...values: number[]) => new Response(new Uint8Array(values))
  return {
    requested,
    headers,
    answer: (url: string, ...values: number[]) => answers.get(url)?.(bytes(...values)),
    answerWith: (url: string, res: Response) => answers.get(url)?.(res),
  }
}

const OTHER = '/api/workspaces/1/spaces/3/docs/other.docx?raw=true'

test('a response for the document that was left is dropped even if the network ignores the abort', async () => {
  const net = heldFetch([RAW, OTHER])
  const { rerender } = render(<FilePreview src={RAW} name="contract.docx" size={1} />)
  await waitFor(() => expect(net.requested).toEqual([RAW]))
  rerender(<FilePreview src={OTHER} name="other.docx" size={1} />)
  await waitFor(() => expect(net.requested).toEqual([RAW, OTHER]))
  await act(async () => net.answer(RAW, 1, 1, 1))
  expect(rendered).not.toHaveBeenCalled()
  expect(screen.queryByTitle(/Preview of/)).toBeNull()
  await act(async () => net.answer(OTHER, 2, 2))
  await waitFor(() => expect(rendered).toHaveBeenCalledOnce())
  expect([...new Uint8Array(rendered.mock.calls[0][0])]).toEqual([2, 2])
  expect(screen.getByTitle('Preview of other.docx')).toBeTruthy()
})

test('a response that arrives after the preview was closed draws nothing', async () => {
  const net = heldFetch([RAW])
  const { unmount } = render(<FilePreview src={RAW} name="contract.docx" size={1} />)
  await waitFor(() => expect(net.requested).toEqual([RAW]))
  unmount()
  await act(async () => net.answer(RAW, 1))
  expect(rendered).not.toHaveBeenCalled()
  expect(document.querySelector('iframe')).toBeNull()
})

test('a file that is bigger than the size it was listed with is refused after the download', async () => {
  const net = heldFetch([RAW])
  render(<FilePreview src={RAW} name="contract.docx" size={10} />)
  await waitFor(() => expect(net.requested).toEqual([RAW]))
  await act(async () => net.answerWith(RAW, new Response(new Uint8Array(MAX_PREVIEW_BYTES + 1))))
  expect(await screen.findByText(/This file is over 20 MB/)).toBeTruthy()
  expect(rendered).not.toHaveBeenCalled()
  expect(screen.queryByTitle('Preview of contract.docx')).toBeNull()
})

test('a file exactly at the limit is previewed', async () => {
  const net = heldFetch([RAW])
  render(<FilePreview src={RAW} name="contract.docx" size={MAX_PREVIEW_BYTES} />)
  await waitFor(() => expect(net.requested).toEqual([RAW]))
  await act(async () => net.answerWith(RAW, new Response(new Uint8Array(MAX_PREVIEW_BYTES))))
  await waitFor(() => expect(rendered).toHaveBeenCalledOnce())
})

test('a frame that is closed while its renderer runs reports nothing afterwards', async () => {
  let finish: () => void = () => {}
  let fail: (e: Error) => void = () => {}
  const done = vi.fn()
  const failed = vi.fn()
  const slow = () => new Promise<void>((resolve) => (finish = resolve))
  const broken = () => new Promise<void>((_, reject) => (fail = reject))
  const first = render(<PreviewFrame title="a" render={slow} onRendered={done} onFailed={failed} />)
  await waitFor(() => expect(finish).not.toBe(undefined))
  await waitFor(() => expect((screen.getByTitle('a') as HTMLIFrameElement).contentDocument?.body).toBeTruthy())
  first.unmount()
  finish()
  await Promise.resolve()
  const second = render(<PreviewFrame title="b" render={broken} onRendered={done} onFailed={failed} />)
  await waitFor(() => expect(document.querySelector('iframe[title="b"]')).toBeTruthy())
  await new Promise((r) => setTimeout(r, 0))
  second.unmount()
  fail(new Error('late'))
  await new Promise((r) => setTimeout(r, 0))
  expect(done).not.toHaveBeenCalled()
  expect(failed).not.toHaveBeenCalled()
})

test('a frame that is reloaded is not drawn into a second time', async () => {
  const draw = vi.fn(async (body: HTMLElement) => {
    body.innerHTML = '<p>x</p>'
  })
  render(<PreviewFrame title="t" render={draw} onRendered={() => {}} onFailed={() => {}} />)
  await waitFor(() => expect(draw).toHaveBeenCalledOnce())
  const el = screen.getByTitle('t') as HTMLIFrameElement
  el.dispatchEvent(new Event('load'))
  el.dispatchEvent(new Event('load'))
  expect(draw).toHaveBeenCalledOnce()
})

test('only a file named .docx is previewed', () => {
  expect(isWord('a/b/Contract.DOCX')).toBe(true)
  for (const other of ['a.doc', 'a.docx.zip', 'docx', 'a.xlsx', 'a.docm', 'a.md']) expect(isWord(other)).toBe(false)
})

test('a failure for the document that was left shows nothing on the one now open', async () => {
  const requested: string[] = []
  const rejects = new Map<string, (e: Error) => void>()
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      requested.push(String(input))
      return new Promise<Response>((_, reject) => rejects.set(String(input), reject))
    }),
  )
  const { rerender } = render(<FilePreview src={RAW} name="contract.docx" size={1} />)
  await waitFor(() => expect(requested).toEqual([RAW]))
  rerender(<FilePreview src={OTHER} name="other.docx" size={1} />)
  await waitFor(() => expect(requested).toEqual([RAW, OTHER]))
  await act(async () => rejects.get(RAW)?.(new TypeError('network')))
  expect(screen.queryByText("This file can't be previewed; download it to open it.")).toBeNull()
  expect(screen.getByRole('status', { name: 'Loading the preview' })).toBeTruthy()
})

test('a second document is drawn by a frame of its own, after the first was drawn', async () => {
  const calls = fakeFetch({ [`GET ${RAW}`]: { body: { a: 1 } }, [`GET ${OTHER}`]: { body: { b: 2 } } })
  rendered.mockImplementation(async (_bytes, into) => {
    into.innerHTML = `<h1>${calls.length === 1 ? 'First' : 'Second'}</h1>`
  })
  const { rerender } = render(<FilePreview src={RAW} name="contract.docx" size={1} />)
  await waitFor(() => expect(frame().contentDocument?.querySelector('h1')?.textContent).toBe('First'))
  rerender(<FilePreview src={OTHER} name="other.docx" size={1} />)
  await waitFor(() => expect(rendered).toHaveBeenCalledTimes(2))
  const next = screen.getByTitle('Preview of other.docx') as HTMLIFrameElement
  await waitFor(() => expect(next.contentDocument?.querySelector('h1')?.textContent).toBe('Second'))
  expect(screen.queryByTitle('Preview of contract.docx')).toBeNull()
})

test('the frame stays hidden behind the loading skeleton until the document is drawn', async () => {
  let finish: () => void = () => {}
  rendered.mockImplementation(
    (_bytes, into) =>
      new Promise<void>((resolve) => {
        into.innerHTML = '<h1>Master agreement</h1>'
        finish = resolve
      }),
  )
  fakeFetch({ [`GET ${RAW}`]: { body: {} } })
  const { container } = render(<FilePreview src={RAW} name="contract.docx" size={1} />)
  await waitFor(() => expect(rendered).toHaveBeenCalledOnce())
  expect(frame().hidden).toBe(true)
  expect(screen.getByRole('status', { name: 'Loading the preview' })).toBeTruthy()
  expect(container.querySelector('.kb-preview')?.getAttribute('aria-busy')).toBe('true')
  await act(async () => finish())
  await waitFor(() => expect(frame().hidden).toBe(false))
  expect(screen.queryByRole('status', { name: 'Loading the preview' })).toBeNull()
  expect(container.querySelector('.kb-preview')?.getAttribute('aria-busy')).toBe('false')
})

test('a load of the frame before its document exists draws nothing and throws nothing', async () => {
  watcher?.disconnect()
  const draw = vi.fn(async () => {})
  const thrown = vi.fn()
  window.addEventListener('error', thrown)
  render(<PreviewFrame title="t" render={draw} onRendered={() => {}} onFailed={() => {}} />)
  const el = screen.getByTitle('t') as HTMLIFrameElement
  Object.defineProperty(el, 'contentDocument', { value: null, configurable: true })
  el.dispatchEvent(new Event('load'))
  window.removeEventListener('error', thrown)
  expect(draw).not.toHaveBeenCalled()
  expect(thrown).not.toHaveBeenCalled()
})

test('a load of the frame whose document has no body yet draws nothing and throws nothing', async () => {
  watcher?.disconnect()
  const draw = vi.fn(async () => {})
  const thrown = vi.fn()
  window.addEventListener('error', thrown)
  render(<PreviewFrame title="t" render={draw} onRendered={() => {}} onFailed={() => {}} />)
  const el = screen.getByTitle('t') as HTMLIFrameElement
  const doc = el.contentDocument as Document
  Object.defineProperty(doc, 'body', { value: null, configurable: true })
  el.dispatchEvent(new Event('load'))
  window.removeEventListener('error', thrown)
  expect(draw).not.toHaveBeenCalled()
  expect(thrown).not.toHaveBeenCalled()
})

test('the frame follows its content with a size observer that ends with it', async () => {
  const observed: Element[] = []
  const disconnect = vi.fn()
  let resize: () => void = () => {}
  vi.stubGlobal(
    'ResizeObserver',
    class {
      constructor(callback: () => void) {
        resize = callback
      }
      observe(el: Element) {
        observed.push(el)
      }
      disconnect = disconnect
    },
  )
  const done = vi.fn()
  const { unmount } = render(<PreviewFrame title="t" render={async () => {}} onRendered={done} onFailed={() => {}} />)
  await waitFor(() => expect(done).toHaveBeenCalledOnce())
  const el = screen.getByTitle('t') as HTMLIFrameElement
  expect(observed).toEqual([el.contentDocument?.body])
  Object.defineProperty(el.contentDocument?.documentElement, 'scrollHeight', { value: 321, configurable: true })
  resize()
  expect(el.style.blockSize).toBe('321px')
  unmount()
  expect(disconnect).toHaveBeenCalledOnce()
})

test('a version in History is previewed from its own commit through Content', async () => {
  const at = '/api/workspaces/1/spaces/3/docs/contract.docx?at=abc123&raw=true'
  const calls = fakeFetch({ [`GET ${at}`]: { body: {} } })
  const doc = { path: 'dir/contract.docx', version: 'a'.repeat(40), size: 12, versioned: true }
  render(<Content url="/api/workspaces/1/spaces/3/docs/contract.docx?at=abc123" doc={doc} onLink={() => null} />)
  await waitFor(() => expect(rendered).toHaveBeenCalledOnce())
  expect(calls.map((c) => c.path)).toEqual([at])
  expect(screen.getByTitle('Preview of contract.docx')).toBeTruthy()
})

test('a new version of the same file is fetched and drawn again, and the old request is ended', async () => {
  rendered.mockImplementation(async (bytes, into) => {
    into.innerHTML = `<h1>${new TextDecoder().decode(bytes)}</h1>`
  })
  let served = 0
  const calls = fakeFetch({
    [`GET ${RAW}`]: () => {
      served += 1
      return { body: { v: served } }
    },
  })
  const url = '/api/workspaces/1/spaces/3/docs/contract.docx'
  const first = { path: 'contract.docx', version: 'a'.repeat(40), size: 7, versioned: true }
  const { rerender } = render(<Content url={url} doc={first} onLink={() => null} />)
  await waitFor(() => expect(frame().contentDocument?.querySelector('h1')?.textContent).toBe('{"v":1}'))
  rerender(<Content url={url} doc={{ ...first, version: 'b'.repeat(40) }} onLink={() => null} />)
  await waitFor(() => expect(frame().contentDocument?.querySelector('h1')?.textContent).toBe('{"v":2}'))
  expect(calls.map((c) => c.path)).toEqual([RAW, RAW])
  expect(calls[0].signal?.aborted).toBe(true)
})

test('a version that is restored while the first is still downloading shows only the restored one', async () => {
  const net = heldFetch([RAW])
  const url = '/api/workspaces/1/spaces/3/docs/contract.docx'
  const first = { path: 'contract.docx', version: 'a'.repeat(40), size: 1, versioned: true }
  const { rerender } = render(<Content url={url} doc={first} onLink={() => null} />)
  await waitFor(() => expect(net.requested).toEqual([RAW]))
  rerender(<Content url={url} doc={{ ...first, version: 'b'.repeat(40) }} onLink={() => null} />)
  await waitFor(() => expect(net.requested).toEqual([RAW, RAW]))
  await act(async () => net.answer(RAW, 2, 2))
  await waitFor(() => expect(rendered).toHaveBeenCalledOnce())
  expect([...new Uint8Array(rendered.mock.calls[0][0])]).toEqual([2, 2])
})

const SIGN_INS: [string, () => void][] = [
  ['signs out', () => endSession()],
  ['signs in as another account in another tab', () => beginSession(8)],
  ['signs in again as the same account', () => beginSession(7)],
]

test.each(SIGN_INS)('bytes that arrive after the person %s draw nothing and show no notice', async (_, change) => {
  beginSession(7)
  const net = heldFetch([RAW])
  render(<FilePreview src={RAW} name="contract.docx" size={1} />)
  await waitFor(() => expect(net.requested).toEqual([RAW]))
  change()
  await act(async () => net.answer(RAW, 1, 2))
  expect(rendered).not.toHaveBeenCalled()
  expect(document.querySelector('iframe')).toBeNull()
  expect(screen.queryByText("This file can't be previewed; download it to open it.")).toBeNull()
})

test.each(SIGN_INS)('a failure that arrives after the person %s shows no notice', async (_, change) => {
  beginSession(7)
  const net = heldFetch([RAW])
  render(<FilePreview src={RAW} name="contract.docx" size={1} />)
  await waitFor(() => expect(net.requested).toEqual([RAW]))
  change()
  await act(async () => net.answerWith(RAW, new Response('{}', { status: 500 })))
  expect(screen.queryByText("This file can't be previewed; download it to open it.")).toBeNull()
  expect(rendered).not.toHaveBeenCalled()
})

test('the download is asked for on behalf of the signed-in account, so another account gets a refusal', async () => {
  beginSession(7)
  const net = heldFetch([RAW])
  render(<FilePreview src={RAW} name="contract.docx" size={1} />)
  await waitFor(() => expect(net.requested).toEqual([RAW]))
  expect(net.headers[0][EXPECTED_USER_HEADER.toLowerCase()]).toBe('7')
})

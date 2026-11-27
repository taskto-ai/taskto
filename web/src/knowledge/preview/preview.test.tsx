// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { fakeFetch } from '../../test/fakes'
import { Content } from '../Content'
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

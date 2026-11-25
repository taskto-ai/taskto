// @vitest-environment jsdom

import { cleanup, fireEvent, render } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { Markdown, safeHref } from './markdown'

afterEach(cleanup)

test('renders the common syntax as elements', () => {
  const { container } = render(
    <Markdown
      text={[
        '# Plan',
        '',
        'Some **bold**, *em*, `code`, ~~gone~~ and snake_case_name.',
        '',
        '- one',
        '- two',
        '  - nested',
        '',
        '3. three',
        '4. four',
        '',
        '> quoted',
        '',
        '```',
        '<b>not bold</b>',
        '```',
        '',
        '---',
      ].join('\n')}
    />,
  )
  const el = container.querySelector('.tt-markdown') as HTMLElement
  expect(el.querySelector('h1')?.textContent).toBe('Plan')
  expect(el.querySelector('strong')?.textContent).toBe('bold')
  expect(el.querySelector('em')?.textContent).toBe('em')
  expect(el.querySelector('p code')?.textContent).toBe('code')
  expect(el.querySelector('del')?.textContent).toBe('gone')
  expect(el.querySelector('p')?.textContent).toContain('snake_case_name')
  expect([...el.querySelectorAll(':scope > ul > li')].map((li) => li.firstChild?.textContent)).toEqual(['one', 'two'])
  expect(el.querySelector('ul ul li')?.textContent).toBe('nested')
  expect(el.querySelector('ol')?.getAttribute('start')).toBe('3')
  expect(el.querySelector('blockquote')?.textContent).toBe('quoted')
  expect(el.querySelector('pre code')?.textContent).toBe('<b>not bold</b>')
  expect(el.querySelector('pre b')).toBeNull()
  expect(el.querySelector('hr')).not.toBeNull()
})

test('raw HTML is shown as text, never made into elements', () => {
  const { container } = render(
    <Markdown
      text={'<script>alert(1)</script>\n\n<img src=x onerror="alert(1)">\n\n<a href="javascript:alert(1)">x</a>'}
    />,
  )
  expect(container.querySelector('script, img, a, iframe')).toBeNull()
  expect(container.textContent).toContain('<script>alert(1)</script>')
  expect(container.textContent).toContain('<img src=x onerror="alert(1)">')
})

test('only http(s), mailto and relative links are links', () => {
  expect(safeHref('https://example.com/a')).toBe('https://example.com/a')
  expect(safeHref('mailto:a@example.com')).toBe('mailto:a@example.com')
  expect(safeHref('notes/plan.md')).toBe('notes/plan.md')
  for (const bad of [
    'javascript:alert(1)',
    ' JavaScript:alert(1)',
    'java\tscript:alert(1)',
    'java\nscript:x',
    'data:text/html,x',
    'vbscript:x',
    '\u0001javascript:x',
  ]) {
    expect(safeHref(bad)).toBeNull()
  }
  // Characters browsers don't skip in a scheme (zero-width, other Unicode spaces) make it no
  // scheme at all: whatever safeHref keeps is, to the browser's own parser, http(s) or mailto.
  for (const odd of [
    'java\u200bscript:alert(1)',
    '\u00a0javascript:x',
    'java\u2028script:x',
    '\\\\evil.example',
    '//evil.example',
  ]) {
    const kept = safeHref(odd)
    if (kept !== null)
      expect(['http:', 'https:', 'mailto:']).toContain(new URL(kept, 'https://taskto.invalid/').protocol)
  }

  const { container } = render(
    <Markdown
      text={
        '[ok](https://example.com) [bad](javascript:alert(1)) [tab](java\tscript:alert(1)) ' +
        '![pic](https://example.com/p.png) <https://example.com/auto> [data](data:text/html,hi)'
      }
    />,
  )
  const links = [...container.querySelectorAll('a')]
  expect(links.map((a) => a.getAttribute('href'))).toEqual([
    'https://example.com',
    'https://example.com/p.png',
    'https://example.com/auto',
  ])
  for (const a of links) {
    expect(a.getAttribute('target')).toBe('_blank')
    expect(a.getAttribute('rel')).toBe('noopener noreferrer')
  }
  // The refused links keep their text; an image never loads.
  expect(container.textContent).toContain('bad')
  expect(container.textContent).toContain('data')
  expect(container.querySelector('img')).toBeNull()
})

test('a relative link is handed to the page, which opens the document', () => {
  const open = vi.fn()
  const onLink = vi.fn((href: string) => (href === '../plan.md' ? open : null))
  const { getByText, container } = render(
    <Markdown text="See [the plan](../plan.md), not [the admin](/w/1/settings)." onLink={onLink} />,
  )
  fireEvent.click(getByText('the plan'))
  expect(onLink).toHaveBeenCalledWith('../plan.md')
  expect(open).toHaveBeenCalledOnce()
  // A link the page has nowhere for is text: no anchor, so nothing to follow away.
  expect(getByText('the admin').closest('a')).toBeNull()
  expect(container.querySelectorAll('a')).toHaveLength(1)
})

test('without a page to handle it, a relative link is text, and a backslashed host is another site', () => {
  const { container } = render(
    <Markdown text={'[the plan](../plan.md) [away](\\\\evil.example/x) [also](/\\evil.example)'} />,
  )
  expect(container.textContent).toContain('the plan')
  const links = [...container.querySelectorAll('a')]
  expect(links.map((a) => a.getAttribute('href'))).toEqual(['\\\\evil.example/x', '/\\evil.example'])
  for (const a of links) expect(a.getAttribute('target')).toBe('_blank')
})

test('deep nesting stays text instead of exhausting the stack', () => {
  const deep = `${'>'.repeat(20000)} quoted\n\n${'['.repeat(5000)}x${'](y)'.repeat(5000)}\n\n${'*_'.repeat(5000)}x${'_*'.repeat(5000)}`
  const { container } = render(<Markdown text={deep} />)
  expect(container.querySelectorAll('blockquote').length).toBeLessThanOrEqual(17)
  expect(container.textContent).toContain('quoted')
})

// The bounded-time tests guard against quadratic parsing, which took 9 to 31 s on these
// inputs: their budget is far above a linear parse on a slow, busy CI runner (about 2 s with
// every core loaded on a laptop) and far below that. Each test may also run past vitest's
// default 5 s timeout.
const BUDGET_MS = 10_000
const TIMEOUT_MS = 60_000

// The time a parse takes is the test process's own CPU time (vitest runs each test file in a
// process of its own), not the wall clock: on a machine busy with other jobs, the wall clock
// also counts the time the test waited for a CPU, and once ran 11 s for a 1 s parse.
function cpuMs(): number {
  const { process } = globalThis as unknown as { process: { cpuUsage(): { user: number; system: number } } }
  const { user, system } = process.cpuUsage()
  return (user + system) / 1000
}

test(
  'a document of unmatched emphasis renders in bounded time',
  () => {
    for (const text of ['*a '.repeat(30_000), 'x _a'.repeat(30_000), '**a '.repeat(30_000)]) {
      const start = cpuMs()
      render(<Markdown text={text} />)
      expect(cpuMs() - start).toBeLessThan(BUDGET_MS)
      cleanup()
    }
  },
  TIMEOUT_MS,
)

test(
  'a document of unclosed brackets renders in bounded time',
  () => {
    const text = '['.repeat(200_000)
    const start = cpuMs()
    const { container } = render(<Markdown text={text} />)
    expect(container.textContent?.length).toBe(200_000)
    expect(cpuMs() - start).toBeLessThan(BUDGET_MS)
  },
  TIMEOUT_MS,
)

// From the conversation view's tests: what models write, as a reply shows it (headings
// lowered by two, line breaks kept).

const md = (text: string) =>
  render(<Markdown text={text} headingShift={2} lineBreaks />).container.firstElementChild as HTMLElement

test('draws what models write', () => {
  const el = md(
    [
      '# Plan',
      '',
      'Some **bold**, *italic*, ~~gone~~ and `code`, in snake_case_names.',
      'A second line.',
      '',
      '- one',
      '- two',
      '  1. nested',
      '  2. again',
      '',
      '3. three',
      '4. four',
      '',
      '> quoted',
      '',
      '```ts',
      'const a = 1 < 2',
      '```',
      '',
      '---',
      '',
      '| Name | Size |',
      '|:-----|-----:|',
      '| a    | 1    |',
    ].join('\n'),
  )
  expect(el.querySelector('h3')?.textContent).toBe('Plan')
  const p = el.querySelector('p') as HTMLElement
  expect(p.querySelector('strong')?.textContent).toBe('bold')
  expect(p.querySelector('em')?.textContent).toBe('italic')
  expect(p.querySelector('del')?.textContent).toBe('gone')
  expect(p.querySelector('code')?.textContent).toBe('code')
  expect(p.textContent).toContain('snake_case_names')
  expect(p.querySelector('br')).toBeTruthy()
  expect([...el.querySelectorAll(':scope > ul > li')].map((li) => li.firstChild?.textContent)).toEqual(['one', 'two'])
  expect([...el.querySelectorAll('ul ol li')].map((li) => li.textContent)).toEqual(['nested', 'again'])
  expect(el.querySelector(':scope > ol')?.getAttribute('start')).toBe('3')
  expect(el.querySelector('blockquote')?.textContent).toBe('quoted')
  const code = el.querySelector('pre code') as HTMLElement
  expect(code.textContent).toBe('const a = 1 < 2')
  expect(code.dataset.lang).toBe('ts')
  expect(el.querySelector('hr')).toBeTruthy()
  expect([...el.querySelectorAll('td')].map((td) => [td.textContent, td.style.textAlign])).toEqual([
    ['a', 'left'],
    ['1', 'right'],
  ])
})

test('raw HTML in a reply is shown as text', () => {
  const attacks = [
    '<script>alert(1)</script>',
    '<img src=x onerror="alert(1)">',
    '<a href="javascript:alert(1)">click</a>',
    '<iframe srcdoc="<script>alert(1)</script>"></iframe>',
    '<svg onload=alert(1)>',
    '**<b onmouseover=alert(1)>bold</b>**',
    '```\n</code></pre><img src=x onerror=alert(1)>\n```',
    '| <img src=x> | b |\n|---|---|\n| <script>x</script> | c |',
    '[<img src=x onerror=alert(1)>](https://example.com)',
  ]
  for (const text of attacks) {
    const el = md(text)
    expect(el.querySelector('script, img, iframe, svg, b, [onerror], [onload], [onmouseover]')).toBeNull()
    // The tags are there, as text.
    expect(el.textContent).toContain('<')
    cleanup()
  }
})

test('links go only to http, https and mailto, and open apart from the page', () => {
  const el = md(
    'See [the docs](https://example.com/a_(b)), <https://example.org>, https://example.net/x. and [mail](mailto:a@example.com).',
  )
  const links = [...el.querySelectorAll('a')]
  expect(links.map((a) => a.getAttribute('href'))).toEqual([
    'https://example.com/a_(b)',
    'https://example.org',
    'https://example.net/x',
    'mailto:a@example.com',
  ])
  for (const a of links) {
    expect(a.getAttribute('target')).toBe('_blank')
    expect(a.getAttribute('rel')).toBe('noopener noreferrer')
  }
})

test('javascript: and other unsafe links are never links', () => {
  const unsafe = [
    '[x](javascript:alert(1))',
    '[x](JaVaScRiPt:alert(1))',
    '[x](  javascript:alert(1))',
    '[x](javascript&#58;alert(1))',
    '[x](java%0ascript:alert(1))',
    '[x](vbscript:msgbox(1))',
    '[x](data:text/html,<script>alert(1)</script>)',
    '[x](file:///etc/passwd)',
    '[x](/api/logout)',
    '<javascript:alert(1)>',
    'javascript:alert(1)',
    '![x](javascript:alert(1))',
  ]
  for (const text of unsafe) {
    const el = md(text)
    expect(el.querySelector('a'), text).toBeNull()
    cleanup()
  }
  for (const href of [
    'javascript:alert(1)',
    ' JAVASCRIPT:x',
    'java\tscript:x',
    'java\nscript:x',
    '\u0000javascript:x',
  ]) {
    expect(safeHref(href), JSON.stringify(href)).toBeNull()
  }
  // An image is a link to it, so a reply never loads anything by itself.
  const img = md('![chart](https://example.com/c.png)')
  expect(img.querySelector('img')).toBeNull()
  expect(img.querySelector('a')?.getAttribute('href')).toBe('https://example.com/c.png')
})

test(
  'hostile input stays fast and shallow',
  () => {
    const start = cpuMs()
    for (const text of [
      '*a '.repeat(20_000),
      '_a '.repeat(20_000),
      '`a '.repeat(20_000),
      '~~a '.repeat(20_000),
      '[a '.repeat(20_000),
      'https://a '.repeat(10_000),
      `${'>'.repeat(5_000)} deep`,
      `${'- '.repeat(2_000)}deep`,
      `${'  '.repeat(2_000)}- deep`,
    ]) {
      md(text)
      cleanup()
    }
    expect(cpuMs() - start).toBeLessThan(BUDGET_MS)
  },
  TIMEOUT_MS,
)

test('task list items are read-only checkboxes, labelled by their text', () => {
  const { container } = render(
    <Markdown
      text={['- [ ] write the post', '- [x] get **approval**', '- [X] ship it', '- plain item', '1. [ ] numbered'].join(
        '\n',
      )}
    />,
  )
  const boxes = [...container.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')]
  expect(boxes.map((b) => b.checked)).toEqual([false, true, true, false])
  expect(boxes.every((b) => b.disabled)).toBe(true)
  // The text is the box's label, so a screen reader names it; the brackets are gone.
  expect(boxes.map((b) => b.closest('label')?.textContent)).toEqual([
    'write the post',
    'get approval',
    'ship it',
    'numbered',
  ])
  expect(container.querySelector('label strong')?.textContent).toBe('approval')
  expect(container.textContent).not.toContain('[')
  expect(container.querySelectorAll('li')).toHaveLength(5)
  expect(container.querySelectorAll('li.tt-task-item')).toHaveLength(4)
})

test('a task item keeps its nested list, and a bracket that is not a box stays text', () => {
  const { container } = render(
    <Markdown
      text={['- [x] parent', '  - [ ] child', '  - child two', '', '- [] no box', '- [ ]', '- [y] nope'].join('\n')}
    />,
  )
  const boxes = container.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')
  expect([...boxes].map((b) => b.checked)).toEqual([true, false])
  expect(container.querySelector('li.tt-task-item ul')).not.toBeNull()
  expect(container.textContent).toContain('[] no box')
  expect(container.textContent).toContain('[ ]')
  expect(container.textContent).toContain('[y] nope')
})

test('a task item makes no HTML either', () => {
  const { container } = render(<Markdown text="- [ ] <img src=x onerror=alert(1)> done" />)
  expect(container.querySelector('img')).toBeNull()
  expect(container.querySelector('label')?.textContent).toBe('<img src=x onerror=alert(1)> done')
})

test('a task item keeps its line breaks in a reply, as an ordinary list item does', () => {
  const { container } = render(
    <Markdown text={['- [ ] First line', '  Second line', '- Plain first', '  plain second'].join('\n')} lineBreaks />,
  )
  const task = container.querySelector('li.tt-task-item label span') as HTMLElement
  expect(task.querySelectorAll('br')).toHaveLength(1)
  expect(task.textContent).toBe('First lineSecond line')
  const plain = [...container.querySelectorAll('li')].find(
    (li) => !li.classList.contains('tt-task-item'),
  ) as HTMLElement
  expect(plain.querySelectorAll('br')).toHaveLength(1)
  // Without line breaks (a document), a task item's lines are one line, as a paragraph's are.
  const doc = render(<Markdown text={['- [ ] First line', '  Second line'].join('\n')} />)
  expect(doc.container.querySelector('li.tt-task-item br')).toBeNull()
})

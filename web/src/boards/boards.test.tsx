// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { fakeFetch } from '../test/fakes'
import { hueClass, labelHue } from '../ui/Label'
import { BoardsPage } from './BoardsPage'
import { localToday } from './due'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.useRealTimers()
  // Undoes hide(): the page is visible again.
  Reflect.deleteProperty(document, 'hidden')
})

const B = '/api/workspaces/1/spaces/3/boards'
const C = `${B}/7/cards`
const by = { id: 2, name: 'Ann', agent: false }

const space = (access: 'write' | 'read') => ({
  id: 3,
  workspace_id: 1,
  kind: 'shared',
  name: 'Team',
  archived_at: null,
  role: access === 'write' ? 'editor' : 'viewer',
  access,
})
const board = { id: 7, space_id: 3, kind: 'team', name: 'Launch', archived_at: null as string | null, version: 1 }
const conversation = (id: number, title: string, archived = false) => ({
  id,
  workspace_id: 1,
  title,
  archived_at: archived ? '2026-01-03T00:00:00Z' : null,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
})

type TestCard = ReturnType<typeof card>
const card = (id: number, title: string, column: string, extra: Record<string, unknown> = {}) => ({
  id,
  board_id: 7,
  title,
  body: '',
  column,
  rank: 'V',
  archived_from: null,
  assignee: null as { kind: string; id?: number; name?: string } | null,
  blocked: false,
  blocked_reason: null as string | null,
  blocked_by: [] as number[],
  docs: [] as string[],
  conversations: [] as string[],
  due: null as string | null,
  labels: [] as string[],
  version: 1,
  updated_at: '2026-01-01T00:00:00Z',
  updated_by: by,
  ...extra,
})

// A board with two cards, kept on the server side of the fake: changes check and bump the
// version as the server does, and answer 409 with the current card for a stale one.
function server(access: 'write' | 'read', tweak?: (cards: TestCard[]) => void) {
  const cards: TestCard[] = [
    card(1, 'Write the post', 'todo', {
      body: 'Some **notes**',
      docs: ['launch/post.md'],
      // 7 is the caller's; 99 someone else's.
      conversations: ['7', '99'],
    }),
    card(2, 'Get approval', 'doing'),
  ]
  tweak?.(cards)
  const boards = [{ ...board }]
  const comments: Record<number, unknown[]> = {
    1: [
      {
        id: 1,
        card_id: 1,
        author: { ...by, agent: true, via: 'laptop' },
        body: 'Started *it*',
        created_at: '2026-01-01T00:00:00Z',
        mentions: [],
      },
    ],
    2: [],
    3: [],
    4: [],
  }
  const update = (id: number, version: number, apply: (c: TestCard) => void) => {
    const c = cards.find((x) => x.id === id) as TestCard
    if (version !== c.version)
      return { status: 409, body: { error: 'it changed since you read it: reload and try again', current: c } }
    apply(c)
    c.version++
    return { body: c }
  }
  const routes: Parameters<typeof fakeFetch>[0] = {
    'GET /api/workspaces/1/spaces': { body: { spaces: [space(access)] } },
    'GET /api/workspaces/1/spaces/3/members': {
      body: {
        members: [
          { id: 2, email: 'ann@example.com', name: 'Ann', role: 'editor', added_at: '2026-01-01T00:00:00Z' },
          { id: 5, email: 'bo@example.com', name: 'Bo', role: 'viewer', added_at: '2026-01-01T00:00:00Z' },
          { id: 6, email: 'cy@example.com', name: 'Cy\\', role: 'viewer', added_at: '2026-01-01T00:00:00Z' },
        ],
      },
    },
    // The caller's own conversations: 7, and 8 archived. Card links to others' show as private.
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation(7, 'Launch plans')] } },
    'GET /api/workspaces/1/conversations?archived=true': { body: { conversations: [conversation(8, 'Old', true)] } },
    [`GET ${B}`]: (call) => ({
      body: { boards: boards.filter((b) => call.path.endsWith('?archived=true') || !b.archived_at) },
    }),
    [`PATCH ${B}/7`]: (call) => {
      const { version, name, archived } = call.body as { version: number; name?: string; archived?: boolean }
      const b = boards[0]
      if (version !== b.version) return { status: 409, body: { error: 'it changed since you read it', current: b } }
      if (name !== undefined) b.name = name
      if (archived !== undefined) b.archived_at = archived ? '2026-01-04T00:00:00Z' : null
      b.version++
      return { body: b }
    },
    [`POST ${B}`]: (call) => ({ status: 201, body: { ...board, id: 8, name: (call.body as { name: string }).name } }),
    [`GET ${B}/8/cards`]: { body: { cards: [] } },
    [`GET ${C}`]: () => ({ body: { cards: cards.filter((c) => c.column !== 'archived') } }),
    [`POST ${C}`]: (call) => {
      const { title, column } = call.body as { title: string; column: string }
      const c = card(cards.length + 1, title, column)
      cards.push(c)
      comments[c.id] = []
      return { status: 201, body: c }
    },
  }
  for (const id of [1, 2, 3, 4]) {
    routes[`GET ${C}/${id}`] = () => ({ body: { ...cards.find((c) => c.id === id), comments: comments[id] } })
    routes[`PATCH ${C}/${id}`] = (call) => {
      const { version, ...fields } = call.body as { version: number } & Partial<TestCard>
      return update(id, version, (c) => {
        // The server's normalization of labels: trimmed, one space, lower case, no repeats.
        if (fields.labels)
          fields.labels = [...new Set(fields.labels.map((l) => l.trim().replace(/\s+/g, ' ').toLowerCase()))].sort()
        // As the server answers: an assigned person with their name.
        if (fields.assignee?.kind === 'user')
          fields.assignee = { ...fields.assignee, name: { 2: 'Ann', 5: 'Bo', 6: 'Cy\\' }[fields.assignee.id ?? 0] }
        Object.assign(c, fields)
        c.blocked = Boolean(c.blocked_reason) || c.blocked_by.length > 0
      })
    }
    // As the server orders a column: before its `position`-th other card, or last.
    routes[`POST ${C}/${id}/move`] = (call) => {
      const { version, column, position } = call.body as { version: number; column: string; position?: number }
      return update(id, version, (c) => {
        cards.splice(cards.indexOf(c), 1)
        c.column = column
        const others = cards.filter((x) => x.column === column)
        const at = position === undefined || position >= others.length ? -1 : cards.indexOf(others[position])
        if (at < 0) cards.push(c)
        else cards.splice(at, 0, c)
      })
    }
    routes[`POST ${C}/${id}/archive`] = (call) =>
      update(id, (call.body as { version: number }).version, (c) => {
        c.column = 'archived'
      })
    routes[`POST ${C}/${id}/comments`] = (call) => {
      const comment = {
        id: 9,
        card_id: id,
        author: by,
        body: (call.body as { body: string }).body,
        created_at: '2026-01-02T00:00:00Z',
        // As the server answers: the names it has for the people the body mentions that the reader sees.
        mentions: [...(call.body as { body: string }).body.matchAll(/\(user:5\)/g)]
          .slice(0, 1)
          .map(() => ({ id: 5, name: 'Bo' })),
      }
      comments[id].push(comment)
      return { status: 201, body: comment }
    }
  }
  const calls = fakeFetch(routes)
  return {
    cards,
    boards,
    comments,
    calls,
    sent: (method: string, path: string) =>
      calls.filter((c) => c.method === method && c.path === path).map((c) => c.body),
  }
}

async function openCard(title: string) {
  fireEvent.click(await screen.findByRole('button', { name: new RegExp(`^${title}`) }))
  return screen.findByRole('region', { name: title })
}

// Opens the menu behind the button named `button` inside `scope`, and chooses `item` in it.
async function choose(scope: HTMLElement, button: string | RegExp, item: string | RegExp) {
  fireEvent.click(await within(scope).findByRole('button', { name: button }))
  fireEvent.click(await within(scope).findByRole('menuitem', { name: item }))
}

// The items of the menu behind the button named `button`, by name; the menu is closed again.
async function menuItems(scope: HTMLElement, button: string | RegExp) {
  const opener = await within(scope).findByRole('button', { name: button })
  fireEvent.click(opener)
  const items = within(scope).getAllByRole('menuitem')
  // As read out: without what is hidden from screen readers (an avatar's initials).
  const names = items.map((i) => {
    const copy = i.cloneNode(true) as HTMLElement
    for (const hidden of copy.querySelectorAll('[aria-hidden="true"]')) hidden.remove()
    return copy.textContent
  })
  fireEvent.keyDown(items[0], { key: 'Escape' })
  return { names, items }
}

// A click as a person makes it: the button takes the focus first (fireEvent.click alone doesn't).
function press(el: HTMLElement) {
  el.focus()
  fireEvent.click(el)
}

// The board's own menu: renaming, a new board, the archived boards, archiving.
const boardMenu = (item: string) => choose(document.body, 'Board actions', item)

test('a viewer reads the board, its cards and their threads, but changes nothing', async () => {
  server('read')
  render(<BoardsPage workspace={1} />)
  const todo = await screen.findByRole('region', { name: 'To do' })
  expect(within(todo).getByText('Write the post')).toBeTruthy()
  expect(screen.queryByRole('button', { name: /New board/ })).toBeNull()
  expect(screen.queryByRole('button', { name: /Add a card/ })).toBeNull()

  const view = await openCard('Write the post')
  expect((await within(view).findByText('notes')).tagName).toBe('STRONG')
  expect(within(view).getByText('Started').parentElement?.textContent).toBe('Started it')
  expect(within(view).getByText(/Ann \(agent, laptop\)/)).toBeTruthy()
  // Its properties are read, not changed: no menu for the status or the assignee.
  expect(within(view).getByText('To do')).toBeTruthy()
  expect(within(view).queryByRole('button', { name: /^Status/ })).toBeNull()
  expect(within(view).queryByRole('button', { name: /^Assignee/ })).toBeNull()
  for (const name of ['Edit', 'Set blocked', 'Card actions', 'Comment', 'Add a label']) {
    expect(within(view).queryByRole('button', { name })).toBeNull()
  }
  expect(within(view).queryByLabelText('Comment')).toBeNull()

  // The links: the viewer's own conversation opens, someone else's is only shown to be there.
  expect(within(view).getByText('launch/post.md')).toBeTruthy()
  const own = await within(view).findByRole('link', { name: 'Launch plans' })
  expect(own.getAttribute('href')).toBe('/w/1/conversations/7')
  expect(within(view).getByText('A conversation only its owner can open')).toBeTruthy()
  expect(within(view).queryByRole('button', { name: /Unlink/ })).toBeNull()
  expect(within(view).queryByRole('form')).toBeNull()
  // The board's menu only lists the archived boards for a viewer.
  expect((await menuItems(document.body, 'Board actions')).names).toEqual(['Show archived boards'])
  // No card moves: not by dragging, and no Move menu.
  const item = within(todo)
    .getByRole('button', { name: /^Write the post/ })
    .closest('li') as HTMLElement
  expect(item.draggable).toBe(false)
  expect(screen.queryByRole('button', { name: /^Move/ })).toBeNull()
  expect(screen.queryByRole('button', { name: 'Add a card' })).toBeNull()
})

test('adds a card to a column and opens it', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Add a card to Review' }))
  const form = screen.getByRole('form', { name: 'New card in Review' })
  fireEvent.change(within(form).getByLabelText('Title'), { target: { value: '  Proofread  ' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Add' }))
  await screen.findByRole('region', { name: 'Proofread' })
  expect(sent('POST', C)).toEqual([{ title: 'Proofread', column: 'review' }])
  const review = screen.getByRole('region', { name: 'Review' })
  await within(review).findByText('Proofread')
})

test('edits a card, moves it to another column, and comments', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  fireEvent.click(await within(view).findByRole('button', { name: 'Edit' }))
  fireEvent.change(within(view).getByLabelText('Title'), { target: { value: 'Write the launch post' } })
  fireEvent.change(within(view).getByLabelText('Description'), { target: { value: 'Draft in `notes`' } })
  press(within(view).getByRole('button', { name: 'Save' }))
  await screen.findByRole('region', { name: 'Write the launch post' })
  expect(sent('PATCH', `${C}/1`)).toEqual([{ version: 1, title: 'Write the launch post', body: 'Draft in `notes`' }])
  // The editor closed: the focus is on its Edit button, not lost to the page.
  await waitFor(() => expect(document.activeElement).toBe(within(view).getByRole('button', { name: 'Edit' })))

  // The next change goes with the version the last one returned.
  await choose(view, 'Status: To do', 'Doing')
  await within(screen.getByRole('region', { name: 'Doing' })).findByText('Write the launch post')
  expect(sent('POST', `${C}/1/move`)).toEqual([{ version: 2, column: 'doing' }])

  fireEvent.change(within(view).getByLabelText('Comment'), { target: { value: 'Ready for **review**' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Comment' }))
  expect((await within(view).findByText('review')).tagName).toBe('STRONG')
  expect(sent('POST', `${C}/1/comments`)).toEqual([{ body: 'Ready for **review**' }])
  expect((within(view).getByLabelText('Comment') as HTMLTextAreaElement).value).toBe('')
})

test('mentions a person from the comment box, where the cursor is, by their ID', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  const field = (await within(view).findByLabelText('Comment')) as HTMLTextAreaElement
  fireEvent.change(field, { target: { value: 'Thanks, please look' } })
  field.setSelectionRange(7, 7)
  await choose(view, 'Mention someone', 'Bo')
  await waitFor(() => expect(field.value).toBe('Thanks, @[Bo](user:5)  please look'))
  fireEvent.click(within(view).getByRole('button', { name: 'Comment' }))
  await waitFor(() => expect(sent('POST', `${C}/1/comments`)).toEqual([{ body: 'Thanks, @[Bo](user:5)  please look' }]))
  // The posted comment shows the name, not the token.
  const posted = await within(view).findByText('@Bo', { selector: 'span.tt-mention' })
  expect(posted.parentElement?.textContent).toContain('Thanks, @Bo')
  expect(view.textContent).not.toContain('user:5')
})

test('shows a mention by the name the server gives for its ID, never by the label in the text', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  const field = (await within(view).findByLabelText('Comment')) as HTMLTextAreaElement
  // The label says Bo, the ID is someone this reader can't see, and the second is in code.
  fireEvent.change(field, {
    target: { value: '@[Bo](user:77) hi @[Bo](user:5), `@[Mal](user:5)` and \\@[Esc](user:5)' },
  })
  fireEvent.click(within(view).getByRole('button', { name: 'Comment' }))
  await waitFor(() => expect(sent('POST', `${C}/1/comments`)).toHaveLength(1))
  const thread = within(view).getByRole('region', { name: 'Comments' })
  await waitFor(() => expect(within(thread).getAllByRole('listitem')).toHaveLength(2))
  const text = within(thread).getAllByRole('listitem')[1].textContent ?? ''
  expect(text).toContain('@Someone hi @Bo,')
  expect(text).toContain('@[Mal](user:5)') // in code, as typed
  // Escaped, it is no mention: only the first two are drawn as ones.
  expect(view.querySelectorAll('.tt-mention')).toHaveLength(2)
  expect(text).not.toContain('@Bo @Bo')
  expect(view.querySelector('.tt-markdown a')).toBeNull()
})

test('a poll replaces the names a held comment shows, and a leading zero in an ID finds the name', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  const { comments } = server('read')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  await within(view).findByText(/Ann \(agent, laptop\)/)
  comments[1].push({ ...comment(20, 'Hi @[Bo](user:05)'), mentions: [{ id: 5, name: 'Bo' }] })
  await act(() => vi.advanceTimersByTimeAsync(10_000))
  await within(view).findByText('@Bo', { selector: 'span.tt-mention' })
  // Bo is renamed: the next read's name replaces the held one.
  comments[1][1] = { ...comment(20, 'Hi @[Bo](user:05)'), mentions: [{ id: 5, name: 'Beatrice' }] }
  await act(() => vi.advanceTimersByTimeAsync(10_000))
  await within(view).findByText('@Beatrice', { selector: 'span.tt-mention' })
  expect(within(view).queryByText('@Bo')).toBeNull()
})

test('a name ending in a backslash is written into the mention without it', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  await choose(view, 'Mention someone', 'Cy\\')
  fireEvent.click(within(view).getByRole('button', { name: 'Comment' }))
  await waitFor(() => expect(sent('POST', `${C}/1/comments`)).toHaveLength(1))
  expect(sent('POST', `${C}/1/comments`)[0]).toEqual({ body: '@[Cy](user:6) ' })
})

test('a comment and a change in flight together both stay', async () => {
  const { sent } = server('write')
  // The comment's answer waits until the test lets it go.
  const answered = globalThis.fetch
  let release = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input).endsWith('/comments') && init?.method === 'POST') await held
    return answered(input, init)
  })
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  fireEvent.change(await within(view).findByLabelText('Comment'), { target: { value: 'On my way' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Comment' }))
  // While the comment is in flight, the card moves.
  await choose(view, 'Status: To do', 'Doing')
  await within(screen.getByRole('region', { name: 'Doing' })).findByText('Write the post')
  await act(async () => release())
  await within(view).findByText('On my way')
  // The comment didn't roll the card back: it is still in Doing, at the version the move made.
  await choose(view, 'Status: Doing', 'Review')
  await within(screen.getByRole('region', { name: 'Review' })).findByText('Write the post')
  expect(sent('POST', `${C}/1/move`)).toEqual([
    { version: 1, column: 'doing' },
    { version: 2, column: 'review' },
  ])
  // And the move didn't drop the comment.
  expect(within(view).getByText('On my way')).toBeTruthy()
})

test('sets a card blocked by a reason and another card, then unblocks it', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  expect(await within(view).findByText('Not blocked.')).toBeTruthy()
  fireEvent.click(within(view).getByRole('button', { name: 'Set blocked' }))
  const form = within(view).getByRole('form', { name: 'Set blocked' })
  fireEvent.change(within(form).getByLabelText('Reason'), { target: { value: 'waiting for legal' } })
  fireEvent.click(within(form).getByLabelText('Get approval'))
  fireEvent.click(within(form).getByRole('button', { name: 'Set blocked' }))
  expect((await within(view).findByText(/Blocked: waiting for legal/)).textContent).toBe(
    'Blocked: waiting for legal (waiting for Get approval)',
  )
  // The board marks it too, with its reason.
  const todo = screen.getByRole('region', { name: 'To do' })
  await waitFor(() =>
    expect(within(todo).getByRole('button', { name: /^Write the post/ }).textContent).toContain('Blocked'),
  )
  expect(within(todo).getByRole('button', { name: /^Write the post/ }).textContent).toContain('waiting for legal')

  fireEvent.click(within(view).getByRole('button', { name: 'Unblock' }))
  await within(view).findByText('Not blocked.')
  expect(sent('PATCH', `${C}/1`)).toEqual([
    { version: 1, blocked_reason: 'waiting for legal', blocked_by: [2] },
    { version: 2, blocked_reason: null, blocked_by: [] },
  ])
})

test('a stale change is shown, and redone over the current card only when asked', async () => {
  const { cards, sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  await within(view).findByRole('button', { name: 'Edit' })
  // Someone else renames it meanwhile.
  Object.assign(cards[0], { title: 'Their title', version: 5 })

  await choose(view, 'Status: To do', 'Review')
  const alert = await within(view).findByRole('alert')
  expect(alert.textContent).toContain('Someone changed this card since you opened it.')
  expect(cards[0].column).toBe('todo')
  // Nothing else can be changed until the conflict is settled: it would replace it unasked.
  const { items } = await menuItems(view, 'Card actions')
  expect(items.find((i) => i.textContent === 'Archive')?.getAttribute('aria-disabled')).toBe('true')
  expect((within(view).getByRole('button', { name: /^Status/ }) as HTMLButtonElement).disabled).toBe(true)

  fireEvent.click(within(alert).getByRole('button', { name: 'Redo my change over it' }))
  await within(screen.getByRole('region', { name: 'Review' })).findByText('Their title')
  expect(sent('POST', `${C}/1/move`)).toEqual([
    { version: 1, column: 'review' },
    { version: 5, column: 'review' },
  ])
  expect(within(view).queryByRole('alert')).toBeNull()

  // Or the writer takes theirs, and their own change is dropped.
  Object.assign(cards.find((c) => c.id === 1) as TestCard, { title: 'Newer title', version: 9 })
  fireEvent.click(within(view).getByRole('button', { name: 'Edit' }))
  fireEvent.change(within(view).getByLabelText('Title'), { target: { value: 'Mine' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Save' }))
  fireEvent.click(await within(view).findByRole('button', { name: 'Load their version' }))
  await screen.findByRole('region', { name: 'Newer title' })
  expect(sent('PATCH', `${C}/1`)).toEqual([{ version: 6, title: 'Mine', body: 'Some **notes**' }])
})

test('creates a team board and shows it', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('region', { name: 'To do' })
  await boardMenu('New board')
  fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Ops' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(sent('POST', B)).toEqual([{ name: 'Ops' }]))
})

test('assigns a card to someone who can read the space, to the agent, and to nobody', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  // The space's members are the choices, after nobody and the agent.
  await waitFor(async () =>
    expect((await menuItems(view, 'Assignee: Nobody')).names).toEqual(['Nobody', 'Agent', 'Ann', 'Bo', 'Cy\\']),
  )
  await choose(view, 'Assignee: Nobody', 'Bo')
  await choose(view, 'Assignee: Bo', 'Agent')
  await choose(view, 'Assignee: Agent', 'Nobody')
  await within(view).findByRole('button', { name: 'Assignee: Nobody' })
  expect(sent('PATCH', `${C}/1`)).toEqual([
    { version: 1, assignee: { kind: 'user', id: 5 } },
    { version: 2, assignee: { kind: 'agent' } },
    { version: 3, assignee: null },
  ])
})

test("links documents and the writer's own conversations, and unlinks any", async () => {
  const { cards, sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  const links = view

  const doc = within(links).getByRole('form', { name: 'Link a document' })
  fireEvent.change(within(doc).getByLabelText('Document path'), { target: { value: ' notes/plan.md ' } })
  fireEvent.click(within(doc).getByRole('button', { name: 'Link' }))
  await within(links).findByText('notes/plan.md')
  await waitFor(() => expect((within(doc).getByLabelText('Document path') as HTMLInputElement).value).toBe(''))

  // Only the writer's own conversations can be linked, archived ones too, and not twice.
  const conv = await within(links).findByRole('form', { name: 'Link a conversation' })
  const choices = within(conv)
    .getAllByRole('option')
    .map((o) => o.textContent)
  expect(choices).toEqual(['Choose one of yours…', 'Old'])
  fireEvent.change(within(conv).getByLabelText('Conversation'), { target: { value: '8' } })
  fireEvent.click(within(conv).getByRole('button', { name: 'Link' }))
  await within(links).findByRole('link', { name: 'Old' })

  // Someone else's conversation can't be opened here, but a writer may unlink it.
  fireEvent.click(within(links).getByRole('button', { name: 'Unlink this conversation' }))
  await waitFor(() => expect(within(links).queryByText('A conversation only its owner can open')).toBeNull())

  // A list change redone after a 409 is made over the current card: what they linked stays.
  Object.assign(cards[0], { docs: [...cards[0].docs, 'theirs.md'], version: 8 })
  fireEvent.click(within(links).getByRole('button', { name: 'Unlink launch/post.md' }))
  const alert = await within(view).findByRole('alert')
  fireEvent.click(within(alert).getByRole('button', { name: 'Redo my change over it' }))
  await within(links).findByText('theirs.md')

  expect(sent('PATCH', `${C}/1`)).toEqual([
    { version: 1, docs: ['launch/post.md', 'notes/plan.md'] },
    { version: 2, conversations: ['7', '99', '8'] },
    { version: 3, conversations: ['7', '8'] },
    { version: 4, docs: ['notes/plan.md'] },
    { version: 8, docs: ['notes/plan.md', 'theirs.md'] },
  ])
})

test('renames and archives a team board, and finds it again among the archived', async () => {
  const { boards, sent } = server('write')
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('heading', { level: 1, name: 'Launch' })
  await boardMenu('Rename')
  const form = screen.getByRole('form', { name: 'Rename the board' })
  fireEvent.change(within(form).getByLabelText('Name'), { target: { value: ' Launch day ' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  await screen.findByRole('heading', { level: 1, name: 'Launch day' })
  await waitFor(() => expect(screen.queryByRole('form', { name: 'Rename the board' })).toBeNull())

  // Archiving asks first, and sends nothing when cancelled.
  await boardMenu('Archive the board')
  const asked = await screen.findByRole('alertdialog')
  expect(asked.textContent).toContain('Archive “Launch day”?')
  fireEvent.click(within(asked).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect(sent('PATCH', `${B}/7`)).toHaveLength(1)

  // Someone else changed it meanwhile: the board is read again, and the archive tried again.
  Object.assign(boards[0], { name: 'Launch week', version: 6 })
  const archive = async () => {
    await boardMenu('Archive the board')
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Archive the board' }))
  }
  await archive()
  expect((await screen.findByRole('alert')).textContent).toContain('Someone changed this board')
  await screen.findByRole('heading', { level: 1, name: 'Launch week' })
  await archive()
  await screen.findByText('This space has no boards yet.')

  await boardMenu('Show archived boards')
  await screen.findByText('This board is archived: it is read-only.')
  expect(screen.queryByRole('button', { name: /Add a card/ })).toBeNull()
  expect((await menuItems(document.body, 'Board actions')).names).not.toContain('Rename')
  await boardMenu('Unarchive the board')
  await screen.findAllByRole('button', { name: /Add a card/ })

  expect(sent('PATCH', `${B}/7`)).toEqual([
    { version: 1, name: 'Launch day' },
    { version: 2, archived: true },
    { version: 6, archived: true },
    { version: 7, archived: false },
  ])
})

test('drags a card before another card, or to the end of a column', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  const item = (title: string) =>
    screen.getByRole('button', { name: new RegExp(`^${title}`) }).closest('li') as HTMLElement
  expect(item('Write the post').draggable).toBe(true)

  // Before "Get approval", at the top of Doing.
  fireEvent.dragStart(item('Write the post'))
  fireEvent.dragOver(screen.getByRole('region', { name: 'Doing' }))
  fireEvent.drop(item('Get approval'))
  const doing = screen.getByRole('region', { name: 'Doing' })
  await within(doing).findByText('Write the post')
  // The open card shows where it went.
  await within(view).findByRole('button', { name: 'Status: Doing' })

  // Onto a column, not a card: last in it.
  fireEvent.dragStart(item('Get approval'))
  fireEvent.drop(screen.getByRole('region', { name: 'Done' }))
  await within(screen.getByRole('region', { name: 'Done' })).findByText('Get approval')

  // Dropped where it already is: nothing to send.
  fireEvent.dragStart(item('Get approval'))
  fireEvent.drop(screen.getByRole('region', { name: 'Done' }))

  // The next change in the card view goes with the version the drag made.
  await choose(view, 'Status: Doing', 'Review')
  await within(screen.getByRole('region', { name: 'Review' })).findByText('Write the post')
  expect(sent('POST', `${C}/1/move`)).toEqual([
    { version: 1, column: 'doing', position: 0 },
    { version: 2, column: 'review' },
  ])
  expect(sent('POST', `${C}/2/move`)).toEqual([{ version: 1, column: 'done' }])
})

// The page as hidden or shown, as when its tab is switched away from and back to.
function hide(hidden: boolean) {
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
  act(() => {
    document.dispatchEvent(new Event('visibilitychange'))
  })
}

const reads = (calls: { method: string; path: string }[]) =>
  calls.filter((c) => c.method === 'GET' && c.path === `${C}/1`).length

const comment = (id: number, body: string) => ({
  id,
  card_id: 1,
  author: { id: 5, name: 'Bo', agent: false },
  body,
  created_at: '2026-01-03T00:00:00Z',
  mentions: [],
})

test("the open card shows others' comments, read again only while the page is visible", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  const { calls, comments } = server('read')
  // While `held` is set, reads of the card wait for it.
  let held: Promise<void> | null = null
  const answered = globalThis.fetch
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    if (held && String(input) === `${C}/1`) await held
    return answered(input, init)
  })
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  await within(view).findByText(/Ann \(agent, laptop\)/)
  expect(reads(calls)).toBe(1)

  comments[1].push(comment(20, 'From **Bo**'))
  await act(() => vi.advanceTimersByTimeAsync(10_000))
  expect((await within(view).findByText('Bo', { selector: '.tt-markdown strong' })).tagName).toBe('STRONG')

  // Hidden, it isn't read.
  hide(true)
  comments[1].push(comment(21, 'While away'))
  await act(() => vi.advanceTimersByTimeAsync(60_000))
  expect(reads(calls)).toBe(2)
  expect(within(view).queryByText('While away')).toBeNull()

  // Shown again, it is read at once.
  hide(false)
  await within(view).findByText('While away')
  expect(reads(calls)).toBe(3)
  // Oldest first, each once.
  const thread = within(view).getByRole('region', { name: 'Comments' })
  expect(within(thread).getAllByRole('listitem')).toHaveLength(3)

  // Hidden while a read is on its way: it lands, and no other read follows.
  let release = () => {}
  held = new Promise((resolve) => {
    release = resolve
  })
  await act(() => vi.advanceTimersByTimeAsync(10_000))
  hide(true)
  await act(async () => release())
  await waitFor(() => expect(reads(calls)).toBe(4))
  await act(() => vi.advanceTimersByTimeAsync(60_000))
  expect(reads(calls)).toBe(4)
})

test('reading the thread backs off after failures, and stops for good when access is lost', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  const { calls } = server('read')
  // GETs of the card fail with this status while it isn't 0.
  let failing = 0
  const answered = globalThis.fetch
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    if (failing && String(input) === `${C}/1` && init?.method === 'GET') {
      calls.push({ method: 'GET', path: `${C}/1`, body: undefined, headers: {} })
      return new Response(JSON.stringify({ error: failing === 404 ? 'not found' : 'boom' }), { status: failing })
    }
    return answered(input, init)
  })
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  await within(view).findByText(/Ann \(agent, laptop\)/)
  const after = async (ms: number, count: number) => {
    await act(() => vi.advanceTimersByTimeAsync(ms))
    await waitFor(() => expect(reads(calls)).toBe(count))
  }

  failing = 500
  await after(10_000, 2) // fails: the next read waits 20 s
  await after(10_000, 2)
  await after(10_000, 3) // fails again: 40 s
  await after(30_000, 3)
  failing = 0
  await after(10_000, 4) // succeeds: back to every 10 s
  await after(10_000, 5)

  // Removed from the space: the card is a 404 now, and it isn't asked for again.
  failing = 404
  await after(10_000, 6)
  expect((await within(view).findByRole('alert')).textContent).toBe("This card's comments stopped updating: not found")
  await after(120_000, 6)
})

test('a link to a board archived since says so, and never opens another board in its place', async () => {
  const { boards } = server('write')
  boards[0].archived_at = '2026-01-04T00:00:00Z'
  boards.push({ ...board, id: 9, name: 'Other board', archived_at: null })
  render(<BoardsPage workspace={1} open="3:7:1" />)
  expect((await screen.findByRole('status')).textContent).toContain('archived or no longer available')
  expect(screen.queryByRole('heading', { name: 'Other board' })).toBeNull()
  expect(screen.queryByText('Write the post')).toBeNull()

  // Looking among the archived boards finds it, and opens the card the link names.
  fireEvent.click(screen.getByRole('button', { name: 'Show archived boards' }))
  await screen.findByText('This board is archived: it is read-only.')
  await screen.findByRole('region', { name: 'Write the post' })
})

test('a link to a board that is gone says so even among the archived', async () => {
  server('write')
  render(<BoardsPage workspace={1} open="3:55:1" />)
  await screen.findByText(/archived or no longer available/)
  await boardMenu('Show archived boards')
  await screen.findByText('The board this link points to is no longer available.')
  expect(screen.queryByRole('heading', { name: 'Launch' })).toBeNull()
})

// Dates far enough from any test run's today: 2001 is always late, 2999 never.
const LATE = '2001-02-03'
const FAR = '2999-12-31'

const withPlanning = (cards: TestCard[]) => {
  Object.assign(cards[0], { due: LATE, labels: ['design', 'launch'] })
  Object.assign(cards[1], { due: FAR, labels: ['launch'] })
  cards.push(card(3, 'Ship it', 'done', { due: LATE, labels: ['design'] }), card(4, 'Plain card', 'backlog'))
}

test('the board shows due dates, late ones marked, and labels', async () => {
  server('read', withPlanning)
  render(<BoardsPage workspace={1} />)
  const todo = await screen.findByRole('region', { name: 'To do' })
  // Late: in the danger tone, and says so. The date is that calendar day, not shifted by a time zone.
  const late = within(todo).getByText('Overdue: Feb 3, 2001')
  expect(late.className).toContain('ui-badge-danger')
  // Each label in the hue its name gives it.
  expect(within(todo).getByText('design').className).toContain(hueClass(labelHue('design')))
  // Later: quiet, not a badge.
  const doing = screen.getByRole('region', { name: 'Doing' })
  const far = within(doing).getByText('Due Dec 31, 2999')
  expect(far.className).not.toContain('ui-badge')
  // A finished card isn't late, however old its date.
  const done = screen.getByRole('region', { name: 'Done' })
  expect(within(done).getByText('Due Feb 3, 2001').className).not.toContain('ui-badge')
  expect(within(screen.getByRole('region', { name: 'Backlog' })).queryByText(/Due|Overdue/)).toBeNull()
})

test('a card due today or tomorrow is due soon, in the warning tone', async () => {
  const today = localToday()
  server('read', (cards) => {
    cards[0].due = today
  })
  render(<BoardsPage workspace={1} />)
  const todo = await screen.findByRole('region', { name: 'To do' })
  const soon = within(todo).getByText(/^Due /)
  expect(soon.className).toContain('ui-badge-warning')
})

test('filters the board by label, and to the overdue cards', async () => {
  server('read', withPlanning)
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('region', { name: 'To do' })
  const filters = screen.getByRole('group', { name: 'Filter the cards' })
  const titlesShown = () =>
    screen
      .getAllByRole('button', { name: /^(Write the post|Get approval|Ship it|Plain card)/ })
      .map((b) => b.textContent?.slice(0, 8))
  expect(titlesShown()).toHaveLength(4)

  expect((await menuItems(filters, 'Label: All labels')).names).toEqual(['All labels', 'design', 'launch'])
  await choose(filters, 'Label: All labels', 'design')
  expect(screen.getByRole('button', { name: /^Write the post/ })).toBeTruthy()
  expect(screen.getByRole('button', { name: /^Ship it/ })).toBeTruthy()
  expect(screen.queryByRole('button', { name: /^Get approval/ })).toBeNull()
  expect(screen.queryByRole('button', { name: /^Plain card/ })).toBeNull()
  // The count of a column is of the cards shown.
  expect(within(screen.getByRole('region', { name: 'Doing' })).getByText('0')).toBeTruthy()
  expect(within(screen.getByRole('region', { name: 'Doing' })).getByText('No cards match.')).toBeTruthy()

  // Together with "overdue only": the done card isn't late, so one card is left.
  fireEvent.click(within(filters).getByLabelText('Overdue only'))
  expect(screen.getByRole('button', { name: /^Write the post/ })).toBeTruthy()
  expect(screen.queryByRole('button', { name: /^Ship it/ })).toBeNull()

  await choose(filters, 'Label: design', 'All labels')
  expect(screen.queryByRole('button', { name: /^Get approval/ })).toBeNull()
  expect(screen.getByRole('button', { name: /^Write the post/ })).toBeTruthy()
})

test('a label filter stays, with no cards, when its last card loses the label', async () => {
  server('write', (cards) => {
    withPlanning(cards)
    cards[2].labels = []
  })
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('region', { name: 'To do' })
  const filters = screen.getByRole('group', { name: 'Filter the cards' })
  await choose(filters, 'Label: All labels', 'design')
  expect(screen.getByRole('button', { name: /^Write the post/ })).toBeTruthy()

  // The card loses its only 'design' label, and the board reloads.
  const view = await openCard('Write the post')
  fireEvent.click(within(view).getByRole('button', { name: 'Remove the label design' }))
  await waitFor(() => expect(within(view).queryByText('design')).toBeNull())
  await waitFor(() => expect(screen.queryByRole('button', { name: /^Write the post/ })).toBeNull())
  // Still chosen, still listed, nothing shown, until it is cleared.
  expect((await menuItems(filters, 'Label: design')).names).toContain('design')
  expect(screen.queryByRole('button', { name: /^Get approval/ })).toBeNull()
  await choose(filters, 'Label: design', 'All labels')
  expect(screen.getByRole('button', { name: /^Get approval/ })).toBeTruthy()
})

test('a board with no due dates or labels has no filters', async () => {
  server('write')
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('region', { name: 'To do' })
  expect(screen.queryByRole('group', { name: 'Filter the cards' })).toBeNull()
})

test('a writer sets and clears the due date, and adds and removes labels, each over the latest card', async () => {
  const { sent } = server('write', withPlanning)
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  const due = within(view).getByLabelText('Due date') as HTMLInputElement
  expect(due.value).toBe(LATE)
  // The field isn't the warning: a badge beside it says the card is late.
  expect(due.getAttribute('aria-invalid')).toBeNull()
  expect(within(view).getByText('Overdue: Feb 3, 2001').className).toContain('ui-badge-danger')

  // Typing a date a part at a time saves nothing, disables nothing and keeps the same field;
  // emptying it isn't clearing the date (the button is), and only Save sends it. Save shows once
  // the draft differs from the date saved.
  const save = () => within(view).queryByRole('button', { name: 'Save the due date' }) as HTMLButtonElement | null
  expect(save()).toBeNull()
  fireEvent.change(due, { target: { value: '' } })
  expect(save()).toBeNull()
  fireEvent.change(due, { target: { value: '0002-01-02' } })
  fireEvent.change(due, { target: { value: '2999-01-02' } })
  expect(sent('PATCH', `${C}/1`)).toEqual([])
  expect(within(view).getByLabelText('Due date')).toBe(due)
  expect(due.disabled).toBe(false)
  expect(due.value).toBe('2999-01-02')
  expect(save()?.disabled).toBe(false)
  press(save() as HTMLButtonElement)
  await waitFor(() => expect(sent('PATCH', `${C}/1`)).toEqual([{ version: 1, due: '2999-01-02' }]))
  await waitFor(() => expect((within(view).getByLabelText('Due date') as HTMLInputElement).value).toBe('2999-01-02'))
  await waitFor(() => expect(save()).toBeNull())
  // Saved, the button went: the focus is back on the field, not lost to the page.
  await waitFor(() => expect(document.activeElement).toBe(due))

  fireEvent.click(within(view).getByRole('button', { name: 'Clear the due date' }))
  await waitFor(() => expect(sent('PATCH', `${C}/1`)[1]).toEqual({ version: 2, due: null }))
  await waitFor(() => expect(within(view).queryByRole('button', { name: 'Clear the due date' })).toBeNull())
  // The cleared date empties the field, which held the saved date.
  await waitFor(() => expect((within(view).getByLabelText('Due date') as HTMLInputElement).value).toBe(''))

  // Labels: sent as typed (the server normalizes them), and the card shows what it returns. The
  // form opens behind the + button, and stays open for the next one.
  fireEvent.click(within(view).getByRole('button', { name: 'Add a label' }))
  const add = async (text: string) => {
    fireEvent.change(within(view).getByLabelText('New label'), { target: { value: text } })
    fireEvent.click(within(view).getByRole('button', { name: 'Add label' }))
  }
  await add('  Needs   REVIEW ')
  await waitFor(() =>
    expect(sent('PATCH', `${C}/1`)[2]).toEqual({ version: 3, labels: ['design', 'launch', 'Needs   REVIEW'] }),
  )
  await within(view).findByText('needs review')
  await add('DESIGN')
  await waitFor(() => expect(sent('PATCH', `${C}/1`)).toHaveLength(4))
  expect(sent('PATCH', `${C}/1`)[3]).toEqual({
    version: 4,
    labels: ['design', 'launch', 'needs review', 'DESIGN'],
  })

  fireEvent.click(within(view).getByRole('button', { name: 'Remove the label launch' }))
  await waitFor(() => expect(sent('PATCH', `${C}/1`)).toHaveLength(5))
  expect(sent('PATCH', `${C}/1`)[4]).toEqual({ version: 5, labels: ['design', 'needs review'] })
  await waitFor(() => expect(within(view).queryByText('launch')).toBeNull())
})

test('saving the due date keeps an unsaved title and description draft', async () => {
  const { sent } = server('write', withPlanning)
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  fireEvent.click(within(view).getByRole('button', { name: 'Edit' }))
  fireEvent.change(within(view).getByLabelText('Description'), { target: { value: 'Half-written' } })
  fireEvent.change(within(view).getByLabelText('Due date'), { target: { value: '2999-01-02' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Save the due date' }))
  await waitFor(() => expect(sent('PATCH', `${C}/1`)).toEqual([{ version: 1, due: '2999-01-02' }]))
  await waitFor(() => expect(sent('PATCH', `${C}/1`)).toHaveLength(1))
  expect((within(view).getByLabelText('Description') as HTMLTextAreaElement).value).toBe('Half-written')
})

test('a failed due date save leaves the draft to save again', async () => {
  const { sent } = server('write', withPlanning)
  const answered = globalThis.fetch
  let failing = true
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    if (failing && init?.method === 'PATCH') {
      failing = false
      return new Response(JSON.stringify({ error: 'no' }), {
        status: 500,
        headers: { 'Content-Type': 'application/json' },
      })
    }
    return answered(input, init)
  })
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  fireEvent.change(within(view).getByLabelText('Due date'), { target: { value: '2999-01-02' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Save the due date' }))
  await within(view).findByRole('alert')
  expect((within(view).getByLabelText('Due date') as HTMLInputElement).value).toBe('2999-01-02')
  await waitFor(() =>
    expect((within(view).getByRole('button', { name: 'Save the due date' }) as HTMLButtonElement).disabled).toBe(false),
  )
  fireEvent.click(within(view).getByRole('button', { name: 'Save the due date' }))
  await waitFor(() => expect(sent('PATCH', `${C}/1`)).toEqual([{ version: 1, due: '2999-01-02' }]))
})

test('a date or label typed while a save is pending is kept', async () => {
  const { sent } = server('write', withPlanning)
  const answered = globalThis.fetch
  let release = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'PATCH') await held
    return answered(input, init)
  })
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  fireEvent.change(within(view).getByLabelText('Due date'), { target: { value: '2999-01-02' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Save the due date' }))
  fireEvent.change(within(view).getByLabelText('Due date'), { target: { value: '2999-03-04' } })
  release()
  await waitFor(() => expect(sent('PATCH', `${C}/1`)).toHaveLength(1))
  await within(view).findByRole('button', { name: 'Clear the due date' })
  await act(async () => {})
  expect((within(view).getByLabelText('Due date') as HTMLInputElement).value).toBe('2999-03-04')
  expect((within(view).getByRole('button', { name: 'Save the due date' }) as HTMLButtonElement).disabled).toBe(false)
})

test('a label typed while a label is being added is kept', async () => {
  const { sent } = server('write', withPlanning)
  const answered = globalThis.fetch
  let release = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'PATCH') await held
    return answered(input, init)
  })
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  fireEvent.click(within(view).getByRole('button', { name: 'Add a label' }))
  const field = within(view).getByLabelText('New label') as HTMLInputElement
  fireEvent.change(field, { target: { value: 'first' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Add label' }))
  fireEvent.change(field, { target: { value: 'second' } })
  release()
  await within(view).findByText('first')
  expect(sent('PATCH', `${C}/1`)).toHaveLength(1)
  expect((within(view).getByLabelText('New label') as HTMLInputElement).value).toBe('second')
})

test('a viewer sees the due date and labels but cannot change them', async () => {
  server('read', withPlanning)
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  const due = within(view).getByText('Overdue: Feb 3, 2001')
  expect(due.className).toContain('ui-badge-danger')
  expect(within(view).getByText('design')).toBeTruthy()
  expect(within(view).queryByLabelText('Due date')).toBeNull()
  expect(within(view).queryByRole('button', { name: 'Add a label' })).toBeNull()
  expect(within(view).queryByRole('button', { name: /Remove the label/ })).toBeNull()
  const plain = await openCard('Plain card')
  expect(within(plain).getByText('No due date.')).toBeTruthy()
  expect(within(plain).getByText('No labels.')).toBeTruthy()
})

test('a card moves by its Move menu, without a mouse, as a drop there would, and the move is said', async () => {
  const { sent } = server('write', (cards) => {
    cards.push(card(3, 'Draft the email', 'todo'), card(4, 'Book the call', 'todo'))
  })
  render(<BoardsPage workspace={1} />)
  const todo = await screen.findByRole('region', { name: 'To do' })

  // The first card of a column can go to the bottom, not the top; the last, the other way round.
  expect((await menuItems(todo, 'Move “Write the post”')).names).toEqual([
    'To the bottom of To do',
    'To Backlog',
    'To Doing',
    'To Review',
    'To Done',
  ])
  expect((await menuItems(todo, 'Move “Book the call”')).names[0]).toBe('To the top of To do')
  expect((await menuItems(todo, 'Move “Book the call”')).names).not.toContain('To the bottom of To do')

  // To the top: before the column's first card, with its position, as a drop on that card.
  await choose(todo, 'Move “Book the call”', 'To the top of To do')
  await waitFor(() => expect(sent('POST', `${C}/4/move`)).toEqual([{ version: 1, column: 'todo', position: 0 }]))
  expect((await screen.findByRole('status')).textContent).toBe('Moved “Book the call” to the top of To do.')

  // To another column: last there, as a drop on the column, with no position.
  await choose(todo, 'Move “Draft the email”', 'To Review')
  const review = screen.getByRole('region', { name: 'Review' })
  const moved = await within(review).findByRole('button', { name: /^Draft the email/ })
  expect(sent('POST', `${C}/3/move`)).toEqual([{ version: 1, column: 'review' }])
  await waitFor(() =>
    expect(screen.getByRole('status').textContent).toBe('Moved “Draft the email” to the bottom of Review.'),
  )
  // The card was drawn again in its new column: the focus went with it, not to the page.
  await waitFor(() => expect(document.activeElement).toBe(moved))
})

test('closing the card gives the focus back to it on the board, wherever it moved', async () => {
  server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  await choose(view, 'Status: To do', 'Doing')
  const doing = screen.getByRole('region', { name: 'Doing' })
  await within(doing).findByRole('button', { name: /^Write the post/ })
  // Escape closes the drawer.
  fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' })
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Write the post' })).toBeNull())
  await waitFor(() =>
    expect(document.activeElement).toBe(within(doing).getByRole('button', { name: /^Write the post/ })),
  )
})

test('the card being viewed is marked on the board', async () => {
  server('read')
  render(<BoardsPage workspace={1} />)
  await openCard('Write the post')
  expect(screen.getByRole('button', { name: /^Write the post/ }).getAttribute('aria-current')).toBe('true')
  expect(screen.getByRole('button', { name: /^Get approval/ }).getAttribute('aria-current')).toBeNull()
})

test("a column's add button opens its form, and Escape closes it with the focus back on the button", async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const add = await screen.findByRole('button', { name: 'Add a card to Done' })
  press(add)
  const form = screen.getByRole('form', { name: 'New card in Done' })
  fireEvent.keyDown(within(form).getByLabelText('Title'), { key: 'Escape' })
  expect(screen.queryByRole('form', { name: 'New card in Done' })).toBeNull()
  await waitFor(() => expect(document.activeElement).toBe(add))
  // The page's Add a card is the first column's.
  fireEvent.click(screen.getByRole('button', { name: 'Add a card' }))
  expect(screen.getByRole('form', { name: 'New card in Backlog' })).toBeTruthy()
  expect(sent('POST', C)).toEqual([])
})

test('the board shows its shape while its cards load', async () => {
  server('read')
  let release = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  const answered = globalThis.fetch
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input) === C) await held
    return answered(input, init)
  })
  const { container } = render(<BoardsPage workspace={1} />)
  await screen.findByRole('heading', { level: 1, name: 'Launch' })
  expect(container.querySelector('[aria-busy="true"]')?.textContent).toBe('Loading…')
  expect(screen.queryByRole('region', { name: 'To do' })).toBeNull()
  await act(async () => release())
  await screen.findByRole('region', { name: 'To do' })
  expect(container.querySelector('[aria-busy="true"]')).toBeNull()
})

test('a shared space with no boards offers a writer to create one, and a viewer nothing', async () => {
  server('write')
  // No boards in the space.
  const answered = globalThis.fetch
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) =>
    String(input) === B ? Response.json({ boards: [] }) : answered(input, init),
  )
  render(<BoardsPage workspace={1} />)
  await screen.findByText('This space has no boards yet.')
  fireEvent.click(screen.getByRole('button', { name: 'New board' }))
  expect(screen.getByRole('form', { name: 'New board' })).toBeTruthy()
  cleanup()

  server('read')
  const readerFetch = globalThis.fetch
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) =>
    String(input) === B ? Response.json({ boards: [] }) : readerFetch(input, init),
  )
  render(<BoardsPage workspace={1} />)
  await screen.findByText('This space has no boards yet.')
  expect(screen.queryByRole('button', { name: 'New board' })).toBeNull()
})

test("another board of the space is chosen from the menu beside the board's name", async () => {
  const { boards } = server('write')
  boards.push({ ...board, id: 8, name: 'Ops' })
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('heading', { level: 1, name: 'Launch' })
  expect((await menuItems(document.body, 'Switch board')).names).toEqual(['Launch', 'Ops'])
  await choose(document.body, 'Switch board', 'Ops')
  await screen.findByRole('heading', { level: 1, name: 'Ops' })
  await screen.findByRole('region', { name: 'To do' })
})

// The confirmation the open card asks before it closes over something unsaved.
const discardAsked = () => screen.queryByRole('alertdialog', { name: 'Discard what you haven’t saved?' })

test('Escape asks before it closes a card over an unsaved description, and Cancel keeps the draft', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  fireEvent.click(await within(view).findByRole('button', { name: 'Edit' }))
  fireEvent.change(within(view).getByLabelText('Description'), { target: { value: 'Half-written' } })
  const preview = within(view).getByRole('radio', { name: 'Preview' })
  fireEvent.click(preview)
  fireEvent.keyDown(preview, { key: 'Escape' })
  const asked = await screen.findByRole('alertdialog', { name: 'Discard what you haven’t saved?' })
  // Cancel is the default: the drawer stays, with the draft.
  expect(document.activeElement).toBe(within(asked).getByRole('button', { name: 'Cancel' }))
  fireEvent.click(within(asked).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(discardAsked()).toBeNull())
  expect(screen.getByRole('region', { name: 'Write the post' })).toBeTruthy()
  fireEvent.click(within(view).getByRole('radio', { name: 'Write' }))
  expect((within(view).getByLabelText('Description') as HTMLTextAreaElement).value).toBe('Half-written')

  // Asked again and discarded: the drawer closes, and nothing was sent.
  fireEvent.keyDown(within(view).getByRole('radio', { name: 'Write' }), { key: 'Escape' })
  fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Discard' }))
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Write the post' })).toBeNull())
  expect(sent('PATCH', `${C}/1`)).toEqual([])
})

test('the close button and another card ask first over a comment being written; nothing unsaved closes at once', async () => {
  server('write')
  render(<BoardsPage workspace={1} />)
  let view = await openCard('Write the post')
  fireEvent.change(await within(view).findByLabelText('Comment'), { target: { value: 'Half a thought' } })

  // Another card on the board: asked, and refused, the first stays open with its draft.
  fireEvent.click(screen.getByRole('button', { name: /^Get approval/ }))
  fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(discardAsked()).toBeNull())
  expect((within(view).getByLabelText('Comment') as HTMLTextAreaElement).value).toBe('Half a thought')
  expect(screen.queryByRole('region', { name: 'Get approval' })).toBeNull()

  // The close button: asked too.
  fireEvent.click(within(view).getByRole('button', { name: 'Close' }))
  expect(await screen.findByRole('alertdialog')).toBeTruthy()
  fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Discard' }))
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Write the post' })).toBeNull())

  // With nothing unsaved, Escape closes it without a question.
  view = await openCard('Get approval')
  fireEvent.keyDown(within(view).getByRole('button', { name: 'Close' }), { key: 'Escape' })
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Get approval' })).toBeNull())
  expect(discardAsked()).toBeNull()
})

test('a label typed into its form, or a reason into the blocked form, is asked about before the card closes', async () => {
  server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  fireEvent.click(within(view).getByRole('button', { name: 'Add a label' }))
  fireEvent.change(within(view).getByLabelText('New label'), { target: { value: 'half' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Close' }))
  fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(discardAsked()).toBeNull())
  fireEvent.click(within(view).getByRole('button', { name: 'Cancel' }))

  fireEvent.click(within(view).getByRole('button', { name: 'Set blocked' }))
  fireEvent.change(within(view).getByLabelText('Reason'), { target: { value: 'waiting' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Close' }))
  expect(await screen.findByRole('alertdialog')).toBeTruthy()
})

test('moving a card to the top and then the bottom of its column says each place', async () => {
  server('write', (cards) => {
    cards.push(card(3, 'Draft the email', 'todo'), card(4, 'Book the call', 'todo'))
  })
  render(<BoardsPage workspace={1} />)
  const todo = await screen.findByRole('region', { name: 'To do' })
  await choose(todo, 'Move “Book the call”', 'To the top of To do')
  await waitFor(() => expect(screen.getByRole('status').textContent).toBe('Moved “Book the call” to the top of To do.'))
  await choose(todo, 'Move “Book the call”', 'To the bottom of To do')
  await waitFor(() =>
    expect(screen.getByRole('status').textContent).toBe('Moved “Book the call” to the bottom of To do.'),
  )
})

test('a drop onto a card in the middle of a column says before which card it went', async () => {
  server('write', (cards) => {
    cards.push(card(3, 'Draft the email', 'todo'), card(4, 'Book the call', 'todo'))
  })
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('region', { name: 'To do' })
  const item = (title: string) =>
    screen.getByRole('button', { name: new RegExp(`^${title}`) }).closest('li') as HTMLElement
  fireEvent.dragStart(item('Book the call'))
  fireEvent.drop(item('Draft the email'))
  await waitFor(() =>
    expect(screen.getByRole('status').textContent).toBe('Moved “Book the call” before “Draft the email” in To do.'),
  )
})

test('with a filter hiding the first card of a column, its top is the top of what is shown, as a drop there', async () => {
  const { sent } = server('write', (cards) => {
    cards.push(
      card(3, 'Draft the email', 'todo', { labels: ['x'] }),
      card(4, 'Book the call', 'todo', { labels: ['x'] }),
    )
  })
  render(<BoardsPage workspace={1} />)
  const filters = await screen.findByRole('group', { name: 'Filter the cards' })
  await choose(filters, 'Label: All labels', 'x')
  const todo = screen.getByRole('region', { name: 'To do' })
  // "Write the post" is hidden above them: the top of what is shown is before "Draft the email".
  await choose(todo, 'Move “Book the call”', 'To the top of To do')
  await waitFor(() => expect(sent('POST', `${C}/4/move`)).toEqual([{ version: 1, column: 'todo', position: 1 }]))
  // The same as a drop on that card.
  const item = (title: string) =>
    screen.getByRole('button', { name: new RegExp(`^${title}`) }).closest('li') as HTMLElement
  await waitFor(() => expect(within(todo).getAllByRole('listitem')[0].textContent).toContain('Book the call'))
  fireEvent.dragStart(item('Draft the email'))
  fireEvent.drop(item('Book the call'))
  await waitFor(() => expect(sent('POST', `${C}/3/move`)).toEqual([{ version: 1, column: 'todo', position: 1 }]))
})

test('the Move menu moves a card into a column that has cards, last there, with no position', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const todo = await screen.findByRole('region', { name: 'To do' })
  await choose(todo, 'Move “Write the post”', 'To Doing')
  const doing = screen.getByRole('region', { name: 'Doing' })
  await waitFor(() =>
    expect(
      within(doing)
        .getAllByRole('listitem')
        .map((li) => li.textContent?.slice(0, 12)),
    ).toEqual(['Get approval', 'Write the po']),
  )
  expect(sent('POST', `${C}/1/move`)).toEqual([{ version: 1, column: 'doing' }])
})

test('after sending a comment the focus is back in the comment box', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  const box = (await within(view).findByLabelText('Comment')) as HTMLTextAreaElement
  fireEvent.change(box, { target: { value: 'Done' } })
  press(within(view).getByRole('button', { name: 'Comment' }))
  await waitFor(() => expect(sent('POST', `${C}/1/comments`)).toHaveLength(1))
  await waitFor(() => expect(box.value).toBe(''))
  expect(document.activeElement).toBe(box)
})

test('moving the open card with its Move menu on the board keeps the focus on it there', async () => {
  server('write')
  render(<BoardsPage workspace={1} />)
  await openCard('Write the post')
  const todo = screen.getByRole('region', { name: 'To do' })
  await choose(todo, 'Move “Write the post”', 'To Review')
  const review = screen.getByRole('region', { name: 'Review' })
  const moved = await within(review).findByRole('button', { name: /^Write the post/ })
  await waitFor(() => expect(document.activeElement).toBe(moved))
})

test('closing a card just archived gives the focus to the column it was in', async () => {
  server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  await choose(view, 'Card actions', 'Archive')
  const todo = screen.getByRole('region', { name: 'To do' })
  await waitFor(() => expect(within(todo).queryByRole('button', { name: /^Write the post/ })).toBeNull())
  fireEvent.click(within(view).getByRole('button', { name: 'Close' }))
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Write the post' })).toBeNull())
  await waitFor(() => expect(todo.contains(document.activeElement)).toBe(true))
})

test('cancelling the label form, or removing a label, puts the focus on Add a label, not on a remove button', async () => {
  server('write', withPlanning)
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  press(within(view).getByRole('button', { name: 'Add a label' }))
  press(within(view).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(document.activeElement).toBe(within(view).getByRole('button', { name: 'Add a label' })))
  press(within(view).getByRole('button', { name: 'Remove the label design' }))
  await waitFor(() => expect(within(view).queryByRole('button', { name: 'Remove the label design' })).toBeNull())
  await waitFor(() => expect(document.activeElement).toBe(within(view).getByRole('button', { name: 'Add a label' })))
})

test('opening a card brings its tile into view beside the drawer', async () => {
  server('read')
  const seen: Element[] = []
  const scroll = vi.fn(function (this: Element) {
    seen.push(this)
  })
  Element.prototype.scrollIntoView = scroll
  try {
    render(<BoardsPage workspace={1} />)
    await openCard('Get approval')
    await waitFor(() => expect(seen).toContain(screen.getByRole('button', { name: /^Get approval/ })))
  } finally {
    Reflect.deleteProperty(Element.prototype, 'scrollIntoView')
  }
})

test("a private space's board menu has no New board and no archiving", async () => {
  server('write')
  const answered = globalThis.fetch
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) =>
    String(input) === '/api/workspaces/1/spaces'
      ? Response.json({ spaces: [{ ...space('write'), kind: 'private', name: 'Private' }] })
      : String(input) === B
        ? Response.json({ boards: [{ ...board, kind: 'personal', name: 'Personal' }] })
        : answered(input, init),
  )
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('heading', { level: 1, name: 'Personal' })
  expect((await menuItems(document.body, 'Board actions')).names).toEqual(['Rename'])
})

test("a card's name for a screen reader is short: its title, whether it is blocked, and when it is due", async () => {
  server('read', (cards) => {
    withPlanning(cards)
    Object.assign(cards[0], { blocked: true, blocked_reason: 'a long reason that the open card says' })
  })
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('region', { name: 'To do' })
  expect(screen.getByRole('button', { name: 'Write the post: blocked and overdue since Feb 3, 2001' })).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Plain card' })).toBeTruthy()
})

test('a card due tomorrow says so in words, in the warning tone', async () => {
  const now = new Date()
  const tomorrow = localToday(new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1))
  server('read', (cards) => {
    cards[0].due = tomorrow
    cards[1].due = localToday()
  })
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('region', { name: 'To do' })
  expect(screen.getByText('Due tomorrow').className).toContain('ui-badge-warning')
  expect(screen.getByText('Due today').className).toContain('ui-badge-warning')
})

test("a card opened from its address is brought into view once the board's cards arrive", async () => {
  server('read')
  const seen: Element[] = []
  Element.prototype.scrollIntoView = vi.fn(function (this: Element) {
    seen.push(this)
  })
  try {
    render(<BoardsPage workspace={1} open="3:7:2" />)
    await screen.findByRole('region', { name: 'Get approval' })
    await waitFor(() => expect(seen).toContain(screen.getByRole('button', { name: /^Get approval/ })))
  } finally {
    Reflect.deleteProperty(Element.prototype, 'scrollIntoView')
  }
})

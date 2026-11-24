// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { fakeFetch } from '../test/fakes'
import { BoardsPage } from './BoardsPage'

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
        Object.assign(c, fields)
        c.blocked = Boolean(c.blocked_reason) || c.blocked_by.length > 0
      })
    }
    routes[`POST ${C}/${id}/move`] = (call) => {
      const { version, column } = call.body as { version: number; column: string }
      return update(id, version, (c) => {
        c.column = column
      })
    }
    routes[`POST ${C}/${id}/comments`] = (call) => {
      const comment = {
        id: 9,
        card_id: id,
        author: by,
        body: (call.body as { body: string }).body,
        created_at: '2026-01-02T00:00:00Z',
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
  fireEvent.click(await screen.findByRole('button', { name: new RegExp(title) }))
  return screen.findByRole('article', { name: title })
}

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
  expect(within(view).queryByLabelText('Column')).toBeNull()
  for (const name of ['Edit', 'Set blocked', 'Archive', 'Comment']) {
    expect(within(view).queryByRole('button', { name })).toBeNull()
  }
  expect(within(view).queryByLabelText('Comment')).toBeNull()

  // The links: the viewer's own conversation opens, someone else's is only shown to be there.
  const links = within(view).getByRole('region', { name: 'Links' })
  expect(within(links).getByText('launch/post.md')).toBeTruthy()
  const own = await within(links).findByRole('link', { name: 'Launch plans' })
  expect(own.getAttribute('href')).toBe('/w/1/conversations/7')
  expect(within(links).getByText('A conversation only its owner can open')).toBeTruthy()
  expect(within(links).queryByRole('button', { name: /Unlink/ })).toBeNull()
  expect(within(links).queryByRole('form')).toBeNull()
  expect(within(view).queryByLabelText('Assignee')).toBeNull()
  expect(screen.queryByRole('button', { name: /Rename|Archive the board/ })).toBeNull()
  const item = within(todo)
    .getByRole('button', { name: /Write the post/ })
    .closest('li') as HTMLElement
  expect(item.draggable).toBe(false)
})

test('adds a card to a column and opens it', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Add a card to Review' }))
  const form = screen.getByRole('form', { name: 'New card in Review' })
  fireEvent.change(within(form).getByLabelText('Title'), { target: { value: '  Proofread  ' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Add' }))
  await screen.findByRole('article', { name: 'Proofread' })
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
  fireEvent.click(within(view).getByRole('button', { name: 'Save' }))
  await screen.findByRole('article', { name: 'Write the launch post' })
  expect(sent('PATCH', `${C}/1`)).toEqual([{ version: 1, title: 'Write the launch post', body: 'Draft in `notes`' }])

  // The next change goes with the version the last one returned.
  fireEvent.change(within(view).getByLabelText('Column'), { target: { value: 'doing' } })
  await within(screen.getByRole('region', { name: 'Doing' })).findByText('Write the launch post')
  expect(sent('POST', `${C}/1/move`)).toEqual([{ version: 2, column: 'doing' }])

  fireEvent.change(within(view).getByLabelText('Comment'), { target: { value: 'Ready for **review**' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Comment' }))
  expect((await within(view).findByText('review')).tagName).toBe('STRONG')
  expect(sent('POST', `${C}/1/comments`)).toEqual([{ body: 'Ready for **review**' }])
  expect((within(view).getByLabelText('Comment') as HTMLTextAreaElement).value).toBe('')
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
  fireEvent.change(within(view).getByLabelText('Column'), { target: { value: 'doing' } })
  await within(screen.getByRole('region', { name: 'Doing' })).findByText('Write the post')
  await act(async () => release())
  await within(view).findByText('On my way')
  // The comment didn't roll the card back: it is still in Doing, at the version the move made.
  expect((within(view).getByLabelText('Column') as HTMLSelectElement).value).toBe('doing')
  fireEvent.change(within(view).getByLabelText('Column'), { target: { value: 'review' } })
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
  // The board marks it too.
  const todo = screen.getByRole('region', { name: 'To do' })
  await waitFor(() => expect(within(todo).getByRole('button', { name: /Write the post/ }).dataset.blocked).toBe('true'))

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

  fireEvent.change(within(view).getByLabelText('Column'), { target: { value: 'review' } })
  const alert = await within(view).findByRole('alert')
  expect(alert.textContent).toContain('Someone changed this card since you opened it.')
  expect(cards[0].column).toBe('todo')
  // Nothing else can be changed until the conflict is settled: it would replace it unasked.
  expect((within(view).getByRole('button', { name: 'Archive' }) as HTMLButtonElement).disabled).toBe(true)

  fireEvent.click(within(alert).getByRole('button', { name: 'Redo my change over it' }))
  await within(screen.getByRole('region', { name: 'Review' })).findByText('Their title')
  expect(sent('POST', `${C}/1/move`)).toEqual([
    { version: 1, column: 'review' },
    { version: 5, column: 'review' },
  ])
  expect(within(view).queryByRole('alert')).toBeNull()

  // Or the writer takes theirs, and their own change is dropped.
  Object.assign(cards[0], { title: 'Newer title', version: 9 })
  fireEvent.click(within(view).getByRole('button', { name: 'Edit' }))
  fireEvent.change(within(view).getByLabelText('Title'), { target: { value: 'Mine' } })
  fireEvent.click(within(view).getByRole('button', { name: 'Save' }))
  fireEvent.click(await within(view).findByRole('button', { name: 'Load their version' }))
  await screen.findByRole('article', { name: 'Newer title' })
  expect(sent('PATCH', `${C}/1`)).toEqual([{ version: 6, title: 'Mine', body: 'Some **notes**' }])
})

test('creates a team board and shows it', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /New board/ }))
  fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Ops' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(sent('POST', B)).toEqual([{ name: 'Ops' }]))
})

test('assigns a card to someone who can read the space, to the agent, and to nobody', async () => {
  const { sent } = server('write')
  render(<BoardsPage workspace={1} />)
  const view = await openCard('Write the post')
  const assignee = (await within(view).findByLabelText('Assignee')) as HTMLSelectElement
  // The space's members are the choices.
  await within(assignee).findByRole('option', { name: 'Bo' })
  fireEvent.change(assignee, { target: { value: 'user:5' } })
  await waitFor(() => expect(assignee.value).toBe('user:5'))
  fireEvent.change(assignee, { target: { value: 'agent' } })
  await waitFor(() => expect(assignee.value).toBe('agent'))
  fireEvent.change(assignee, { target: { value: '' } })
  await waitFor(() => expect(assignee.value).toBe(''))
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
  const links = within(view).getByRole('region', { name: 'Links' })

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
  fireEvent.click(await screen.findByRole('button', { name: 'Rename' }))
  const form = screen.getByRole('form', { name: 'Rename the board' })
  fireEvent.change(within(form).getByLabelText('Name'), { target: { value: ' Launch day ' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Save' }))
  await screen.findByText('Launch day', { selector: 'strong' })
  expect(screen.queryByRole('form', { name: 'Rename the board' })).toBeNull()

  // Someone else changed it meanwhile: the board is read again, and the archive tried again.
  Object.assign(boards[0], { name: 'Launch week', version: 6 })
  fireEvent.click(screen.getByRole('button', { name: 'Archive the board' }))
  expect((await screen.findByRole('alert')).textContent).toContain('Someone changed this board')
  await screen.findByText('Launch week', { selector: 'strong' })
  fireEvent.click(screen.getByRole('button', { name: 'Archive the board' }))
  await screen.findByText('This space has no boards yet.')

  fireEvent.click(screen.getByLabelText('Show archived boards'))
  await screen.findByText('This board is archived: it is read-only.')
  expect(screen.queryByRole('button', { name: /Add a card/ })).toBeNull()
  expect(screen.queryByRole('button', { name: 'Rename' })).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Unarchive the board' }))
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
  const item = (title: string) => screen.getByRole('button', { name: new RegExp(title) }).closest('li') as HTMLElement
  expect(item('Write the post').draggable).toBe(true)

  // Before "Get approval", at the top of Doing.
  fireEvent.dragStart(item('Write the post'))
  fireEvent.dragOver(screen.getByRole('region', { name: 'Doing' }))
  fireEvent.drop(item('Get approval'))
  const doing = screen.getByRole('region', { name: 'Doing' })
  await within(doing).findByText('Write the post')
  // The open card shows where it went.
  await waitFor(() => expect((within(view).getByLabelText('Column') as HTMLSelectElement).value).toBe('doing'))

  // Onto a column, not a card: last in it.
  fireEvent.dragStart(item('Get approval'))
  fireEvent.drop(screen.getByRole('region', { name: 'Done' }))
  await within(screen.getByRole('region', { name: 'Done' })).findByText('Get approval')

  // Dropped where it already is: nothing to send.
  fireEvent.dragStart(item('Get approval'))
  fireEvent.drop(screen.getByRole('region', { name: 'Done' }))

  // The next change in the card view goes with the version the drag made.
  fireEvent.change(within(view).getByLabelText('Column'), { target: { value: 'review' } })
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
  expect((await within(view).findByText('Bo', { selector: 'strong' })).tagName).toBe('STRONG')

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
  expect(screen.queryByText('Other board', { selector: 'strong' })).toBeNull()
  expect(screen.queryByText('Write the post')).toBeNull()

  // Looking among the archived boards finds it, and opens the card the link names.
  fireEvent.click(screen.getByRole('button', { name: 'Show archived boards' }))
  await screen.findByText('This board is archived: it is read-only.')
  await screen.findByRole('article', { name: 'Write the post' })
})

test('a link to a board that is gone says so even among the archived', async () => {
  server('write')
  render(<BoardsPage workspace={1} open="3:55:1" />)
  await screen.findByText(/archived or no longer available/)
  fireEvent.click(screen.getByLabelText('Show archived boards'))
  await screen.findByText('The board this link points to is no longer available.')
  expect(screen.queryByText('Launch', { selector: 'strong' })).toBeNull()
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
  // Late: marked, and says so. The date is that calendar day, not shifted by a time zone.
  const late = within(todo).getByText('Overdue: Feb 3, 2001')
  expect(late.getAttribute('data-overdue')).toBe('true')
  expect(within(todo).getByText('design')).toBeTruthy()
  const doing = screen.getByRole('region', { name: 'Doing' })
  const far = within(doing).getByText('Due Dec 31, 2999')
  expect(far.getAttribute('data-overdue')).toBeNull()
  // A finished card isn't late, however old its date.
  const done = screen.getByRole('region', { name: 'Done' })
  expect(within(done).getByText('Due Feb 3, 2001').getAttribute('data-overdue')).toBeNull()
  expect(within(screen.getByRole('region', { name: 'Backlog' })).queryByText(/Due|Overdue/)).toBeNull()
})

test('filters the board by label, and to the overdue cards', async () => {
  server('read', withPlanning)
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('region', { name: 'To do' })
  const filters = screen.getByRole('group', { name: 'Filter the cards' })
  const titlesShown = () =>
    screen
      .getAllByRole('button', { name: /Write the post|Get approval|Ship it|Plain card/ })
      .map((b) => b.textContent?.slice(0, 8))
  expect(titlesShown()).toHaveLength(4)

  const label = within(filters).getByLabelText('Label')
  expect([...(label as HTMLSelectElement).options].map((o) => o.textContent)).toEqual([
    'All labels',
    'design',
    'launch',
  ])
  fireEvent.change(label, { target: { value: 'design' } })
  expect(screen.getByRole('button', { name: /Write the post/ })).toBeTruthy()
  expect(screen.getByRole('button', { name: /Ship it/ })).toBeTruthy()
  expect(screen.queryByRole('button', { name: /Get approval/ })).toBeNull()
  expect(screen.queryByRole('button', { name: /Plain card/ })).toBeNull()
  // The count of a column is of the cards shown.
  expect(within(screen.getByRole('region', { name: 'Doing' })).getByText('0')).toBeTruthy()
  expect(within(screen.getByRole('region', { name: 'Doing' })).getByText('No cards match.')).toBeTruthy()

  // Together with "overdue only": the done card isn't late, so one card is left.
  fireEvent.click(within(filters).getByLabelText('Overdue only'))
  expect(screen.getByRole('button', { name: /Write the post/ })).toBeTruthy()
  expect(screen.queryByRole('button', { name: /Ship it/ })).toBeNull()

  fireEvent.change(label, { target: { value: '' } })
  expect(screen.queryByRole('button', { name: /Get approval/ })).toBeNull()
  expect(screen.getByRole('button', { name: /Write the post/ })).toBeTruthy()
})

test('a label filter stays, with no cards, when its last card loses the label', async () => {
  server('write', (cards) => {
    withPlanning(cards)
    cards[2].labels = []
  })
  render(<BoardsPage workspace={1} />)
  await screen.findByRole('region', { name: 'To do' })
  const label = screen.getByLabelText('Label') as HTMLSelectElement
  fireEvent.change(label, { target: { value: 'design' } })
  expect(screen.getByRole('button', { name: /Write the post/ })).toBeTruthy()

  // The card loses its only 'design' label, and the board reloads.
  const view = await openCard('Write the post')
  fireEvent.click(within(view).getByRole('button', { name: 'Remove the label design' }))
  await waitFor(() => expect(within(view).queryByText('design')).toBeNull())
  await waitFor(() => expect(screen.queryByRole('button', { name: /Write the post/ })).toBeNull())
  // Still chosen, still listed, nothing shown, until it is cleared.
  expect(label.value).toBe('design')
  expect([...label.options].map((o) => o.textContent)).toContain('design')
  expect(screen.queryByRole('button', { name: /Get approval/ })).toBeNull()
  fireEvent.change(label, { target: { value: '' } })
  expect(screen.getByRole('button', { name: /Get approval/ })).toBeTruthy()
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
  expect(due.getAttribute('data-overdue')).toBe('true')

  // Typing a date a part at a time saves nothing, disables nothing and keeps the same field;
  // emptying it isn't clearing the date (the button is), and only Save sends it.
  const save = within(view).getByRole('button', { name: 'Save the due date' }) as HTMLButtonElement
  expect(save.disabled).toBe(true)
  fireEvent.change(due, { target: { value: '' } })
  fireEvent.change(due, { target: { value: '0002-01-02' } })
  fireEvent.change(due, { target: { value: '2999-01-02' } })
  expect(sent('PATCH', `${C}/1`)).toEqual([])
  expect(within(view).getByLabelText('Due date')).toBe(due)
  expect(due.disabled).toBe(false)
  expect(due.value).toBe('2999-01-02')
  expect(save.disabled).toBe(false)
  fireEvent.click(save)
  await waitFor(() => expect(sent('PATCH', `${C}/1`)).toEqual([{ version: 1, due: '2999-01-02' }]))
  await waitFor(() => expect((within(view).getByLabelText('Due date') as HTMLInputElement).value).toBe('2999-01-02'))
  await waitFor(() => expect(save.disabled).toBe(true))

  fireEvent.click(within(view).getByRole('button', { name: 'Clear the due date' }))
  await waitFor(() => expect(sent('PATCH', `${C}/1`)[1]).toEqual({ version: 2, due: null }))
  await waitFor(() => expect(within(view).queryByRole('button', { name: 'Clear the due date' })).toBeNull())
  // The cleared date empties the field, which held the saved date.
  expect((within(view).getByLabelText('Due date') as HTMLInputElement).value).toBe('')

  // Labels: sent as typed (the server normalizes them), and the card shows what it returns.
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
  expect(due.getAttribute('data-overdue')).toBe('true')
  expect(within(view).getByText('design')).toBeTruthy()
  expect(within(view).queryByLabelText('Due date')).toBeNull()
  expect(within(view).queryByLabelText('New label')).toBeNull()
  expect(within(view).queryByRole('button', { name: /Remove the label/ })).toBeNull()
  const plain = await openCard('Plain card')
  expect(within(plain).getByText('No due date.')).toBeTruthy()
  expect(within(plain).getByText('No labels.')).toBeTruthy()
})

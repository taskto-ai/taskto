// Boards: a space's boards, the chosen board's cards by column, and the open card. Writers
// add, rename and archive boards, add and edit cards, assign them, link documents and
// conversations to them, move them between columns (with the Column field, or by dragging
// them), comment, mark them blocked, and give them a due date and labels (the board can be
// narrowed to a label, or to the overdue cards); viewers only read. Every change sends the version
// last read: if someone changed the card meanwhile, the server answers 409 with the card as
// it is, and the writer chooses to take that or to redo their change over it. The open
// card's thread is polled, so others' comments appear. On a phone it is the board or the
// open card.

import { type DragEvent, type FormEvent, useEffect, useRef, useState } from 'react'
import {
  ApiError,
  type Author,
  api,
  type Board,
  type Card,
  type CardWithThread,
  type Comment,
  type Conversation,
  errorText,
  type Me,
  type Space,
  type SpaceMember,
  ws,
} from '../api'
import { conversationTitle } from '../conversations/ConversationView'
import { type Key, type T, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Markdown, type MentionNames } from '../markdown'
import { Link, parseBoardItem, routePath } from '../router'
import { Icon } from '../shell/Icon'
import { SpacePicker, useSpaces } from '../spaces'
import { isOverdue, useToday } from './due'
import { usePoll } from './poll'

// Every column's name, the archived one included.
const COLUMN_LABELS: Record<Card['column'], Key> = {
  backlog: 'boards.columns.backlog',
  todo: 'boards.columns.todo',
  doing: 'boards.columns.doing',
  review: 'boards.columns.review',
  done: 'boards.columns.done',
  archived: 'boards.columns.archived',
}

// The columns of the board, in order.
const COLUMNS = (['backlog', 'todo', 'doing', 'review', 'done'] as const).map((id) => ({
  id,
  label: COLUMN_LABELS[id],
}))

// Why a card is blocked, as one sentence: its reason, the cards it waits for (a list made
// in the user's language, "" for none), or both.
function blockedText(t: T, reason: string | null, cards: string): string {
  if (reason && cards) return t('boards.blockedBecauseWaiting', { reason, cards })
  if (reason) return t('boards.blockedBecause', { reason })
  if (cards) return t('boards.blockedWaiting', { cards })
  return t('boards.blocked')
}

// open is a link's item (boardItem): the space, board and card to open on.
export function BoardsPage({ workspace, open }: { workspace: number; open?: string }) {
  const target = parseBoardItem(open)
  const { spaces, space, choose } = useSpaces(workspace, target?.space)
  const base = space ? `${ws(workspace)}/spaces/${space.id}/boards` : null
  // Archived boards are listed only when asked for, after the others.
  const [archived, setArchived] = useState(false)
  const boards = useLoad(
    () =>
      base
        ? api<{ boards: Board[] }>(`${base}${archived ? '?archived=true' : ''}`).then((r) => r.boards)
        : Promise.resolve(undefined),
    [base, archived],
  )
  const [chosen, setChosen] = useState<number | null>(target?.board ?? null)
  const [adding, setAdding] = useState(false)
  const [renaming, setRenaming] = useState(false)
  const [boardError, setBoardError] = useState<string | null>(null)
  // A link names its board: if that one isn't among those listed (it was archived since, or
  // is gone) another board is not shown in its place, which would read as the card's board.
  const linkedMissing =
    boards.data !== undefined &&
    target?.board !== undefined &&
    chosen === target.board &&
    !boards.data.some((b) => b.id === chosen)
  const board = linkedMissing ? undefined : (boards.data?.find((b) => b.id === chosen) ?? boards.data?.[0])
  const canWrite = space?.access === 'write'
  const t = useT()

  // Renames, archives or unarchives the board, with the version last read. A 409 means
  // someone changed it meanwhile: the list is read again, to do it over the current board.
  const changeBoard = async (b: Board, fields: { name?: string; archived?: boolean }) => {
    setBoardError(null)
    try {
      await api<Board>(`${base}/${b.id}`, { method: 'PATCH', body: { version: b.version, ...fields } })
      setRenaming(false)
    } catch (err) {
      setBoardError(err instanceof ApiError && err.status === 409 ? t('boards.boardChanged') : errorText(err))
    }
    boards.reload()
  }

  return (
    <div className="tt-section">
      <div className="tt-pane-head">
        <h1>{t('shell.sections.boards')}</h1>
        {spaces.data && (
          <SpacePicker
            spaces={spaces.data}
            space={space}
            choose={(id) => {
              setAdding(false)
              setRenaming(false)
              setBoardError(null)
              choose(id)
            }}
          />
        )}
        {boards.data && boards.data.length > 1 && (
          <label className="tt-inline-field">
            {t('boards.board')}
            <select
              value={board?.id ?? ''}
              onChange={(e) => {
                setRenaming(false)
                setBoardError(null)
                setChosen(Number(e.target.value))
              }}
            >
              {!board && (
                <option value="" disabled>
                  {t('boards.board')}
                </option>
              )}
              {boards.data.map((b) => (
                <option key={b.id} value={b.id}>
                  {b.archived_at ? t('boards.archivedBoard', { name: b.name }) : b.name}
                </option>
              ))}
            </select>
          </label>
        )}
        {/* A private space has only its personal board, which can't be archived. */}
        {canWrite && space?.kind === 'shared' && (
          <button type="button" className="tt-button" onClick={() => setAdding(true)}>
            <Icon name="plus" />
            {t('boards.newBoard')}
          </button>
        )}
        {space?.kind === 'shared' && (
          <label className="tt-inline-field">
            <input type="checkbox" checked={archived} onChange={(e) => setArchived(e.target.checked)} />
            {t('boards.showArchived')}
          </label>
        )}
      </div>
      {(spaces.error || boards.error) && (
        <p role="alert" className="tt-error">
          {spaces.error ?? boards.error}
        </p>
      )}
      {linkedMissing && (
        <div role="status" className="tt-panel">
          <p>{t(archived ? 'boards.linkedUnavailable' : 'boards.linkedArchivedOrGone')}</p>
          {!archived && space?.kind === 'shared' && (
            <button type="button" className="tt-button" onClick={() => setArchived(true)}>
              {t('boards.showArchived')}
            </button>
          )}
        </div>
      )}
      {board && (
        <div className="tt-actions">
          <strong>{board.name}</strong>
          {board.archived_at && <span className="tt-muted">{t('boards.archivedNotice')}</span>}
          {canWrite && !board.archived_at && (
            <button type="button" className="tt-button" onClick={() => setRenaming(true)}>
              <Icon name="edit" />
              {t('boards.rename')}
            </button>
          )}
          {canWrite && board.kind === 'team' && (
            <button
              type="button"
              className="tt-button"
              onClick={() => changeBoard(board, { archived: !board.archived_at })}
            >
              <Icon name="archive" />
              {board.archived_at ? t('boards.unarchiveBoard') : t('boards.archiveBoard')}
            </button>
          )}
        </div>
      )}
      {board && renaming && (
        <RenameBoard
          board={board}
          onSave={(name) => changeBoard(board, { name })}
          onCancel={() => setRenaming(false)}
        />
      )}
      {boardError && (
        <p role="alert" className="tt-error">
          {boardError}
        </p>
      )}
      {base && adding && (
        <NewBoard
          url={base}
          onCreated={(b) => {
            setAdding(false)
            setChosen(b.id)
            boards.reload()
          }}
          onCancel={() => setAdding(false)}
        />
      )}
      {spaces.data?.length === 0 && <p className="tt-muted">{t('boards.noSpaces')}</p>}
      {(spaces.data === undefined || base) && boards.data === undefined && !boards.error && !spaces.error && (
        <p className="tt-muted">{t('app.loading')}</p>
      )}
      {boards.data?.length === 0 && !linkedMissing && <p className="tt-muted">{t('boards.noBoards')}</p>}
      {base && space && board && (
        <BoardView
          key={board.id}
          url={`${base}/${board.id}/cards`}
          workspace={workspace}
          space={space}
          canWrite={canWrite && !board.archived_at}
          openCard={board.id === target?.board ? target.card : undefined}
        />
      )}
    </div>
  )
}

function RenameBoard({
  board,
  onSave,
  onCancel,
}: {
  board: Board
  onSave: (name: string) => Promise<void>
  onCancel: () => void
}) {
  const [name, setName] = useState(board.name)
  const [busy, setBusy] = useState(false)
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    await onSave(name.trim())
    setBusy(false)
  }
  return (
    <form className="tt-panel tt-form" aria-label={t('boards.renameBoard')} onSubmit={submit}>
      <label>
        {t('boards.name')}
        <input
          value={name}
          maxLength={100}
          onChange={(e) => setName(e.target.value)}
          // biome-ignore lint/a11y/noAutofocus: the form was opened to type this
          autoFocus
        />
      </label>
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !name.trim()}>
          {t('boards.save')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

function NewBoard({ url, onCreated, onCancel }: { url: string; onCreated: (b: Board) => void; onCancel: () => void }) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      onCreated(await api<Board>(url, { method: 'POST', body: { name: name.trim() } }))
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return (
    <form className="tt-panel tt-form" aria-label={t('boards.newBoard')} onSubmit={submit}>
      <label>
        {t('boards.name')}
        <input
          value={name}
          maxLength={100}
          onChange={(e) => setName(e.target.value)}
          // biome-ignore lint/a11y/noAutofocus: the form was opened to type this
          autoFocus
        />
      </label>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !name.trim()}>
          {t('boards.create')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

function BoardView({
  url,
  workspace,
  space,
  canWrite,
  openCard,
}: {
  url: string
  workspace: number
  space: Space
  canWrite: boolean
  // The card to open on, a link's.
  openCard?: number
}) {
  const cards = useLoad(() => api<{ cards: Card[] }>(url).then((r) => r.cards), [url])
  const [open, setOpen] = useState<number | null>(openCard ?? null)
  // Dragging a card: which one, and the column it is over. The Column field of the open card
  // does the same from the keyboard, so dragging is only a quicker way for a pointer.
  const [dragged, setDragged] = useState<Card | null>(null)
  const [over, setOver] = useState<Card['column'] | null>(null)
  // The open card as a drag moved it, for the card view to show.
  const [moved, setMoved] = useState<Card | null>(null)
  const [error, setError] = useState<string | null>(null)
  // The board's filters, applied to the cards already loaded: a label, and only the overdue.
  const [label, setLabel] = useState('')
  const [overdueOnly, setOverdueOnly] = useState(false)
  const today = useToday()
  const t = useT()
  const f = useFormat()
  if (cards.error && !cards.data)
    return (
      <p role="alert" className="tt-error">
        {cards.error}
      </p>
    )
  if (!cards.data) return <p className="tt-muted">{t('app.loading')}</p>

  // Drops the dragged card in `column`, before `before` or last; position counts the column's
  // cards without it, as the server does.
  const drop = async (e: DragEvent, column: Card['column'], before?: Card) => {
    e.preventDefault()
    e.stopPropagation()
    const c = dragged
    setDragged(null)
    setOver(null)
    if (!c || before?.id === c.id) return
    const others = (cards.data ?? []).filter((x) => x.column === column && x.id !== c.id)
    const position = before ? others.findIndex((x) => x.id === before.id) : others.length
    const now = (cards.data ?? []).filter((x) => x.column === column).findIndex((x) => x.id === c.id)
    if (c.column === column && position === now) return
    setError(null)
    try {
      const updated = await api<Card>(`${url}/${c.id}/move`, {
        method: 'POST',
        body: { version: c.version, column, ...(before ? { position } : {}) },
      })
      if (updated.id === open) setMoved(updated)
    } catch (err) {
      setError(
        err instanceof ApiError && err.status === 409 ? t('boards.cardMoved', { title: c.title }) : errorText(err),
      )
    }
    cards.reload()
  }
  const dragOver = (e: DragEvent, column: Card['column']) => {
    if (!dragged) return
    e.preventDefault()
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move'
    setOver(column)
  }

  // The chosen label stays chosen, and listed, when no card has it any more (a reload took
  // its last card): the board shows no cards until the person clears the filter.
  const allLabels = [...new Set([...cards.data.flatMap((c) => c.labels), ...(label ? [label] : [])])].sort()
  const shownLabel = label
  const filtering = shownLabel !== '' || overdueOnly
  const shown = (c: Card) => (!shownLabel || c.labels.includes(shownLabel)) && (!overdueOnly || isOverdue(c, today))
  const filterable = allLabels.length > 0 || cards.data.some((c) => c.due !== null) || filtering

  return (
    <div className="tt-board" data-open={open ? 'card' : 'board'}>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {filterable && (
        <fieldset className="tt-board-filters" aria-label={t('boards.filters')}>
          {allLabels.length > 0 && (
            <label className="tt-inline-field">
              {t('boards.filterLabel')}
              <select value={shownLabel} onChange={(e) => setLabel(e.target.value)}>
                <option value="">{t('boards.allLabels')}</option>
                {allLabels.map((l) => (
                  <option key={l} value={l}>
                    {l}
                  </option>
                ))}
              </select>
            </label>
          )}
          <label className="tt-inline-field">
            <input type="checkbox" checked={overdueOnly} onChange={(e) => setOverdueOnly(e.target.checked)} />
            {t('boards.overdueOnly')}
          </label>
        </fieldset>
      )}
      <div className="tt-columns">
        {COLUMNS.map((col) => {
          const inColumn = cards.data?.filter((c) => c.column === col.id && shown(c)) ?? []
          return (
            <section
              key={col.id}
              className="tt-column"
              aria-label={t(col.label)}
              data-drop={(dragged && over === col.id) || undefined}
              onDragOver={(e) => dragOver(e, col.id)}
              onDrop={(e) => drop(e, col.id)}
            >
              <h2>
                {t(col.label)} <span className="tt-muted">{f.number(inColumn.length)}</span>
              </h2>
              <ul>
                {inColumn.map((c) => (
                  <li
                    key={c.id}
                    draggable={canWrite}
                    data-dragged={dragged?.id === c.id || undefined}
                    onDragStart={(e) => {
                      if (!canWrite) return
                      // Some browsers start a drag only with data set.
                      e.dataTransfer?.setData('text/plain', c.title)
                      if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move'
                      setDragged(c)
                    }}
                    onDragEnd={() => {
                      setDragged(null)
                      setOver(null)
                    }}
                    onDrop={(e) => drop(e, col.id, c)}
                  >
                    <button
                      type="button"
                      className="tt-card"
                      data-blocked={c.blocked || undefined}
                      aria-current={c.id === open || undefined}
                      onClick={() => setOpen(c.id)}
                    >
                      <span>{c.title}</span>
                      {c.due && (
                        <span className="tt-due tt-small" data-overdue={isOverdue(c, today) || undefined}>
                          {t(isOverdue(c, today) ? 'boards.overdueOn' : 'boards.dueOn', {
                            date: f.calendarDate(c.due),
                          })}
                        </span>
                      )}
                      {c.labels.length > 0 && (
                        <span className="tt-labels">
                          {c.labels.map((l) => (
                            <span key={l} className="tt-label tt-small">
                              {l}
                            </span>
                          ))}
                        </span>
                      )}
                      {c.blocked && <span className="tt-blocked tt-small">{t('boards.blocked')}</span>}
                      {c.assignee && <span className="tt-muted tt-small">{assigneeLabel(t, c.assignee)}</span>}
                    </button>
                  </li>
                ))}
              </ul>
              {filtering && inColumn.length === 0 && <p className="tt-muted tt-small">{t('boards.noMatches')}</p>}
              {canWrite && (
                <AddCard
                  url={url}
                  column={col}
                  onAdded={(c) => {
                    cards.reload()
                    setOpen(c.id)
                  }}
                />
              )}
            </section>
          )
        })}
      </div>
      {open && (
        <CardView
          key={open}
          url={`${url}/${open}`}
          workspace={workspace}
          space={space}
          board={cards.data}
          canWrite={canWrite}
          moved={moved?.id === open ? moved : null}
          onChanged={cards.reload}
          onClose={() => setOpen(null)}
        />
      )}
    </div>
  )
}

const assigneeLabel = (t: T, a: NonNullable<Card['assignee']>) =>
  a.kind === 'agent'
    ? t('boards.agent')
    : a.kind === 'deleted'
      ? t('common.deletedUser')
      : (a.name ?? t('boards.assigned'))
const authorLabel = (t: T, a: Author) =>
  !a
    ? t('boards.someone')
    : !a.agent
      ? a.name
      : a.via
        ? t('boards.byAgentVia', { name: a.name, via: a.via })
        : t('boards.byAgent', { name: a.name })

type Person = { id: number; name: string }

// Who a card can be assigned to: whoever can read its space, so the owner of a private space,
// or the members of a shared one.
const readers = (workspace: number, space: Space): Promise<Person[]> =>
  space.kind === 'private'
    ? api<Me>('/api/me').then((me) => [{ id: me.user.id, name: me.user.name }])
    : api<{ members: SpaceMember[] }>(`${ws(workspace)}/spaces/${space.id}/members`).then((r) => r.members)

function AddCard({
  url,
  column,
  onAdded,
}: {
  url: string
  column: (typeof COLUMNS)[number]
  onAdded: (c: Card) => void
}) {
  const [open, setOpen] = useState(false)
  const [title, setTitle] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  if (!open) {
    return (
      <button
        type="button"
        className="tt-button tt-add-card"
        aria-label={t('boards.addCardTo', { column: t(column.label) })}
        onClick={() => setOpen(true)}
      >
        <Icon name="plus" />
        {t('boards.addCard')}
      </button>
    )
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const card = await api<Card>(url, { method: 'POST', body: { title: title.trim(), column: column.id } })
      setTitle('')
      setOpen(false)
      onAdded(card)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="tt-form" aria-label={t('boards.newCardIn', { column: t(column.label) })} onSubmit={submit}>
      <input
        aria-label={t('boards.cardTitle')}
        value={title}
        maxLength={200}
        onChange={(e) => setTitle(e.target.value)}
        // biome-ignore lint/a11y/noAutofocus: the form was opened to type this
        autoFocus
      />
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !title.trim()}>
          {t('boards.add')}
        </button>
        <button type="button" className="tt-button" onClick={() => setOpen(false)}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

// A change to the card, made over the card it is given: with its version, and from its lists
// for a change to one (the server replaces a list whole), so redoing it over the current card
// keeps what others added.
type Send = (base: Card) => Promise<Card>
type Fields = Record<string, unknown> | ((base: Card) => Record<string, unknown>)

// A thread with a newer read's comments added, oldest first: new ones are added, and the read's copy
// replaces the held one with the same ID if it differs (the names its mentions have can change);
// comments held only here stay. The same array if nothing differs, so nothing renders again.
function withComments(have: Comment[], read: Comment[]): Comment[] {
  const server = new Map(read.map((c) => [c.id, c]))
  let changed = false
  const merged = have.map((c) => {
    const fresh = server.get(c.id)
    if (!fresh || JSON.stringify(fresh) === JSON.stringify(c)) return c
    changed = true
    return fresh
  })
  const known = new Set(have.map((c) => c.id))
  const added = read.filter((c) => !known.has(c.id))
  return !changed && added.length === 0 ? have : [...merged, ...added].sort((a, b) => a.id - b.id)
}

// The open card: its fields, its thread, and the changes a writer can make. Each change is
// sent with the version shown; a 409 carries the card as it is now, and the writer either
// takes it (their change is dropped) or sends the same change again on top of it. The thread
// is read again every few seconds while the page is visible, for others' comments; the
// card's fields aren't, so a form being filled in never changes under the writer (a stale
// change still meets the 409).
function CardView({
  url,
  workspace,
  space,
  board,
  canWrite,
  moved,
  onChanged,
  onClose,
}: {
  url: string
  workspace: number
  space: Space
  board: Card[]
  canWrite: boolean
  moved: Card | null
  onChanged: () => void
  onClose: () => void
}) {
  const loaded = useLoad(() => api<CardWithThread>(url), [url])
  const [local, setLocal] = useState<CardWithThread | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [conflict, setConflict] = useState<{ current: Card; send: Send } | null>(null)
  const [editing, setEditing] = useState(false)
  const card = local ?? loaded.data
  const first = loaded.data
  const t = useT()
  const people = useLoad(
    () => (canWrite ? readers(workspace, space) : Promise.resolve([])),
    [workspace, space.id, space.kind, canWrite],
  )

  const stopped = usePoll(
    first ? () => api<CardWithThread>(url) : null,
    (read) =>
      // Onto the latest card, like every change: only the thread is taken from the read.
      setLocal((latest) => {
        const base = latest ?? first
        if (!base) return latest
        const comments = withComments(base.comments, read.comments)
        return comments === base.comments ? latest : { ...base, comments }
      }),
    [url, first],
  )

  // The card was dragged to another column: show it there, unless the writer is editing it,
  // whose save then meets the 409 and chooses.
  useEffect(() => {
    if (!moved || editing) return
    setLocal((latest) => {
      const base = latest ?? first
      return base && moved.version > base.version ? { ...moved, comments: base.comments } : latest
    })
  }, [moved, editing, first])

  // keepEditor: a change from elsewhere on the card (its due date, labels) doesn't close the
  // editor of the title and description, whose unsaved draft would be lost.
  const change = async (send: Send, base: Card | undefined = card, keepEditor = false): Promise<boolean> => {
    if (!card || !base) return false
    setBusy(true)
    setError(null)
    try {
      const updated = await send(base)
      // Onto the latest card: a comment added while this was in flight stays.
      setLocal((latest) => ({ ...updated, comments: (latest ?? card).comments }))
      setConflict(null)
      if (!keepEditor) setEditing(false)
      onChanged()
      return true
    } catch (err) {
      const current = err instanceof ApiError && err.status === 409 && (err.body as { current?: Card })?.current
      if (current) setConflict({ current, send })
      else setError(errorText(err))
      return false
    } finally {
      setBusy(false)
    }
  }
  const takeTheirs = (current: Card) => {
    if (card) setLocal((latest) => ({ ...current, comments: (latest ?? card).comments }))
    setConflict(null)
    setEditing(false)
    onChanged()
  }

  const patch = (fields: Fields) => (base: Card) =>
    api<Card>(url, {
      method: 'PATCH',
      body: { version: base.version, ...(typeof fields === 'function' ? fields(base) : fields) },
    })
  const post =
    (action: string, fields: Record<string, unknown> = {}) =>
    (base: Card) =>
      api<Card>(`${url}/${action}`, { method: 'POST', body: { version: base.version, ...fields } })

  return (
    <article className="tt-card-view" aria-label={card ? card.title : t('boards.card')}>
      <div className="tt-pane-head">
        <button type="button" className="tt-icon-button tt-back" aria-label={t('boards.back')} onClick={onClose}>
          <Icon name="back" />
        </button>
        <h2>{card?.title ?? t('boards.card')}</h2>
        <button type="button" className="tt-button" onClick={onClose}>
          {t('boards.close')}
        </button>
      </div>
      {loaded.error && !card && (
        <p role="alert" className="tt-error">
          {loaded.error}
        </p>
      )}
      {!card && !loaded.error && <p className="tt-muted">{t('app.loading')}</p>}
      {conflict && (
        <div role="alert" className="tt-conflict">
          <span>{t('boards.cardChanged')}</span>
          <button type="button" className="tt-button" onClick={() => takeTheirs(conflict.current)}>
            {t('boards.takeTheirs')}
          </button>
          <button
            type="button"
            className="tt-button tt-danger"
            disabled={busy}
            onClick={() => change(conflict.send, conflict.current)}
          >
            {t('boards.redoMine')}
          </button>
        </div>
      )}
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {stopped && (
        <p role="alert" className="tt-error">
          {t('boards.pollStopped', { error: stopped })}
        </p>
      )}
      {card && (
        <>
          <div className="tt-card-meta">
            {canWrite && card.column !== 'archived' ? (
              <label className="tt-inline-field">
                {t('boards.column')}
                <select
                  value={card.column}
                  disabled={busy || conflict !== null}
                  onChange={(e) => change(post('move', { column: e.target.value }))}
                >
                  {COLUMNS.map((c) => (
                    <option key={c.id} value={c.id}>
                      {t(c.label)}
                    </option>
                  ))}
                </select>
              </label>
            ) : (
              <span className="tt-muted">{t(COLUMN_LABELS[card.column])}</span>
            )}
            {canWrite ? (
              <Assignee
                card={card}
                people={people.data}
                busy={busy || conflict !== null}
                onSet={(assignee) => change(patch({ assignee }))}
              />
            ) : (
              card.assignee && (
                <span className="tt-muted">{t('boards.assignedTo', { name: assigneeLabel(t, card.assignee) })}</span>
              )
            )}
            {canWrite &&
              (card.column === 'archived' ? (
                <button
                  type="button"
                  className="tt-button"
                  disabled={busy || conflict !== null}
                  onClick={() => change(post('restore'))}
                >
                  {t('boards.restore')}
                </button>
              ) : (
                <button
                  type="button"
                  className="tt-button"
                  disabled={busy || conflict !== null}
                  onClick={() => change(post('archive'))}
                >
                  {t('boards.archive')}
                </button>
              ))}
          </div>
          {editing ? (
            <EditCard
              // Per card, not per version: a due date or label saved meanwhile keeps the draft.
              key={card.id}
              card={card}
              busy={busy || conflict !== null}
              onSave={(fields) => change(patch(fields))}
              onCancel={() => setEditing(false)}
            />
          ) : (
            <>
              {card.body.trim() ? (
                <Markdown text={card.body} />
              ) : (
                <p className="tt-muted">{canWrite ? t('boards.noDescriptionYet') : t('boards.noDescription')}</p>
              )}
              {canWrite && (
                <div className="tt-actions">
                  <button type="button" className="tt-button" onClick={() => setEditing(true)}>
                    {t('boards.edit')}
                  </button>
                </div>
              )}
            </>
          )}
          <Blocked
            key={`blocked-${card.version}`}
            card={card}
            board={board}
            canWrite={canWrite}
            busy={busy || conflict !== null}
            onSet={(reason, by) => change(patch({ blocked_reason: reason, blocked_by: by }))}
          />
          <Planning
            card={card}
            canWrite={canWrite}
            busy={busy || conflict !== null}
            onChange={(fields) => change(patch(fields), card, true)}
          />
          <Links
            workspace={workspace}
            card={card}
            canWrite={canWrite}
            busy={busy || conflict !== null}
            onChange={(fields) => change(patch(fields))}
          />
          <Thread
            url={`${url}/comments`}
            comments={card.comments}
            canWrite={canWrite}
            people={people.data}
            onAdded={(c) =>
              // Onto the latest card: a change saved while the comment was in flight stays.
              setLocal((latest) => {
                const base = latest ?? card
                return { ...base, comments: [...base.comments, c] }
              })
            }
          />
        </>
      )}
    </article>
  )
}

function EditCard({
  card,
  busy,
  onSave,
  onCancel,
}: {
  card: Card
  busy: boolean
  onSave: (fields: { title: string; body: string }) => void
  onCancel: () => void
}) {
  const [title, setTitle] = useState(card.title)
  const [body, setBody] = useState(card.body)
  const [tab, setTab] = useState<'write' | 'preview'>('write')
  const t = useT()
  return (
    <form
      className="tt-editor"
      aria-label={t('boards.editCard')}
      onSubmit={(e) => {
        e.preventDefault()
        onSave({ title: title.trim(), body })
      }}
    >
      <input
        aria-label={t('boards.cardTitle')}
        value={title}
        maxLength={200}
        onChange={(e) => setTitle(e.target.value)}
      />
      <fieldset className="tt-view-tabs" aria-label={t('boards.view')}>
        <button type="button" aria-pressed={tab === 'write'} onClick={() => setTab('write')}>
          {t('boards.write')}
        </button>
        <button type="button" aria-pressed={tab === 'preview'} onClick={() => setTab('preview')}>
          {t('boards.preview')}
        </button>
      </fieldset>
      {tab === 'write' ? (
        <textarea
          aria-label={t('boards.description')}
          value={body}
          rows={8}
          onChange={(e) => setBody(e.target.value)}
        />
      ) : (
        <div className="tt-preview">
          <Markdown text={body} />
        </div>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !title.trim()}>
          {t('boards.save')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

// Why the card is blocked: a reason, other cards of the board it waits for, or both.
function Blocked({
  card,
  board,
  canWrite,
  busy,
  onSet,
}: {
  card: Card
  board: Card[]
  canWrite: boolean
  busy: boolean
  onSet: (reason: string | null, by: number[]) => void
}) {
  const [editing, setEditing] = useState(false)
  const [reason, setReason] = useState(card.blocked_reason ?? '')
  const [by, setBy] = useState<number[]>(card.blocked_by)
  const others = board.filter((c) => c.id !== card.id)
  const t = useT()
  const f = useFormat()
  const title = (id: number) => board.find((c) => c.id === id)?.title ?? t('boards.cardNumber', { id: String(id) })

  if (!editing) {
    return (
      <section className="tt-blocked-section" aria-label={t('boards.blocked')}>
        {card.blocked ? (
          <p className="tt-blocked">{blockedText(t, card.blocked_reason, f.list(card.blocked_by.map(title)))}</p>
        ) : (
          <p className="tt-muted">{t('boards.notBlocked')}</p>
        )}
        {canWrite && card.column !== 'archived' && (
          <div className="tt-actions">
            <button type="button" className="tt-button" disabled={busy} onClick={() => setEditing(true)}>
              {card.blocked ? t('boards.changeBlocked') : t('boards.setBlocked')}
            </button>
            {card.blocked && (
              <button type="button" className="tt-button" disabled={busy} onClick={() => onSet(null, [])}>
                {t('boards.unblock')}
              </button>
            )}
          </div>
        )}
      </section>
    )
  }
  return (
    <form
      className="tt-panel tt-form"
      aria-label={t('boards.setBlocked')}
      onSubmit={(e) => {
        e.preventDefault()
        onSet(reason.trim() || null, by)
      }}
    >
      <label>
        {t('boards.reason')}
        <input value={reason} placeholder={t('boards.reasonPlaceholder')} onChange={(e) => setReason(e.target.value)} />
      </label>
      {others.length > 0 && (
        <fieldset className="tt-checks">
          <legend>{t('boards.waitingFor')}</legend>
          {others.map((c) => (
            <label key={c.id}>
              <input
                type="checkbox"
                checked={by.includes(c.id)}
                onChange={(e) => setBy((b) => (e.target.checked ? [...b, c.id] : b.filter((id) => id !== c.id)))}
              />
              {c.title}
            </label>
          ))}
        </fieldset>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || (!reason.trim() && by.length === 0)}>
          {t('boards.setBlocked')}
        </button>
        <button type="button" className="tt-button" onClick={() => setEditing(false)}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

// The card's due date and labels. A due date is a calendar day, so it reads the same wherever
// it is seen; it is late from the day after, for a card that isn't done. Labels are free text
// the server normalizes (Unicode form, case, spaces), so what people type is sent as it is and
// the card shown is the server's. A label is added or removed over the latest card's, so
// redoing the change keeps the labels others set. The due date is a draft until it is saved
// (with Enter or the button), so typing a date a part at a time never saves the part.
function Planning({
  card,
  canWrite,
  busy,
  onChange,
}: {
  card: Card
  canWrite: boolean
  busy: boolean
  onChange: (fields: Fields) => Promise<boolean>
}) {
  const [due, setDue] = useState(card.due ?? '')
  const [label, setLabel] = useState('')
  // The draft follows the card's date when that changes (a save, or someone else's edit),
  // unless it has been edited since: a date typed while a save was pending stays to be saved.
  const synced = useRef(card.due ?? '')
  useEffect(() => {
    const next = card.due ?? ''
    const previous = synced.current
    synced.current = next
    setDue((draft) => (draft === previous ? next : draft))
  }, [card.due])
  const t = useT()
  const f = useFormat()
  const editable = canWrite && card.column !== 'archived'
  const today = useToday()
  const overdue = isOverdue(card, today)
  const typed = label.trim()
  const add = async (e: FormEvent) => {
    e.preventDefault()
    if (!typed) return
    // The server normalizes and removes repeats; the field empties once it has saved.
    // Only if it still holds what was sent: a label typed meanwhile stays.
    const sent = label
    if (await onChange((base) => ({ labels: [...base.labels, typed] }))) setLabel((now) => (now === sent ? '' : now))
  }
  const saveDue = (e: FormEvent) => {
    e.preventDefault()
    // Emptying a part of the field gives '': clearing is the button's.
    if (due && due !== card.due) void onChange({ due })
  }
  return (
    <>
      <div className="tt-planning">
        {editable ? (
          <form className="tt-actions" onSubmit={saveDue}>
            <label className="tt-inline-field">
              {t('boards.dueDate')}
              <input
                type="date"
                value={due}
                data-overdue={overdue || undefined}
                onChange={(e) => setDue(e.target.value)}
              />
            </label>
            <button type="submit" className="tt-button" disabled={busy || !due || due === card.due}>
              {t('boards.saveDue')}
            </button>
            {card.due && (
              <button type="button" className="tt-button" disabled={busy} onClick={() => onChange({ due: null })}>
                {t('boards.clearDue')}
              </button>
            )}
          </form>
        ) : card.due ? (
          <p className="tt-due" data-overdue={overdue || undefined}>
            {t(overdue ? 'boards.overdueOn' : 'boards.dueOn', { date: f.calendarDate(card.due) })}
          </p>
        ) : (
          <p className="tt-muted">{t('boards.noDue')}</p>
        )}
      </div>
      <section className="tt-planning" aria-label={t('boards.labels')}>
        {card.labels.length > 0 ? (
          <ul className="tt-labels">
            {card.labels.map((l) => (
              <li key={l} className="tt-label">
                {l}
                {editable && (
                  <button
                    type="button"
                    className="tt-icon-button"
                    aria-label={t('boards.removeLabel', { label: l })}
                    disabled={busy}
                    onClick={() => onChange((base) => ({ labels: base.labels.filter((x) => x !== l) }))}
                  >
                    <Icon name="close" />
                  </button>
                )}
              </li>
            ))}
          </ul>
        ) : (
          <p className="tt-muted">{t('boards.noLabels')}</p>
        )}
        {editable && (
          <form className="tt-actions" onSubmit={add}>
            <input
              aria-label={t('boards.labelField')}
              value={label}
              maxLength={30}
              placeholder={t('boards.labelPlaceholder')}
              onChange={(e) => setLabel(e.target.value)}
            />
            <button type="submit" className="tt-button" disabled={busy || !typed}>
              {t('boards.addLabel')}
            </button>
          </form>
        )}
      </section>
    </>
  )
}

// Who the card is for: nobody, the agent, or someone who can read the space. Someone assigned
// who has since lost access stays listed until the assignee is changed.
function Assignee({
  card,
  people,
  busy,
  onSet,
}: {
  card: Card
  people: Person[] | undefined
  busy: boolean
  onSet: (assignee: Card['assignee']) => void
}) {
  const a = card.assignee
  const t = useT()
  const value = !a ? '' : a.kind === 'deleted' ? 'deleted' : a.kind === 'agent' ? 'agent' : `user:${a.id}`
  const listed = people ?? []
  const others =
    a?.kind === 'user' && !listed.some((p) => p.id === a.id) ? [{ id: a.id, name: assigneeLabel(t, a) }] : []
  return (
    <label className="tt-inline-field">
      {t('boards.assignee')}
      <select
        value={value}
        disabled={busy}
        onChange={(e) => {
          const v = e.target.value
          onSet(v === '' ? null : v === 'agent' ? { kind: 'agent' } : { kind: 'user', id: Number(v.slice(5)) })
        }}
      >
        {/* The account that was assigned is gone: shown as it is, not pickable; choosing anyone else replaces it. */}
        {a?.kind === 'deleted' && (
          <option value="deleted" disabled>
            {t('common.deletedUser')}
          </option>
        )}
        <option value="">{t('boards.nobody')}</option>
        <option value="agent">{t('boards.agent')}</option>
        {[...others, ...listed].map((p) => (
          <option key={p.id} value={`user:${p.id}`}>
            {p.name}
          </option>
        ))}
      </select>
    </label>
  )
}

// What the card links to: documents of its space, by path, and conversations. A conversation
// is private to its owner: only they can link it or open it, and the others see only that the
// card links one. Writers may unlink any.
function Links({
  workspace,
  card,
  canWrite,
  busy,
  onChange,
}: {
  workspace: number
  card: Card
  canWrite: boolean
  busy: boolean
  onChange: (fields: (base: Card) => Partial<Pick<Card, 'docs' | 'conversations'>>) => Promise<boolean>
}) {
  const needed = canWrite || card.conversations.length > 0
  // The caller's own conversations, archived ones too: the ones they may open or link.
  const mine = useLoad(
    () =>
      needed
        ? Promise.all(
            ['', '?archived=true'].map((q) =>
              api<{ conversations: Conversation[] }>(`${ws(workspace)}/conversations${q}`).then((r) => r.conversations),
            ),
          ).then((lists) => lists.flat())
        : Promise.resolve([]),
    [workspace, needed],
  )
  const [path, setPath] = useState('')
  const [chosen, setChosen] = useState('')
  const t = useT()
  const own = (id: string) => mine.data?.find((c) => String(c.id) === id)
  const linkable = (mine.data ?? []).filter((c) => !card.conversations.includes(String(c.id)))

  const linkDoc = async (e: FormEvent) => {
    e.preventDefault()
    const p = path.trim()
    if (await onChange((base) => ({ docs: [...base.docs, p] }))) setPath('')
  }
  const linkConversation = async (e: FormEvent) => {
    e.preventDefault()
    if (await onChange((base) => ({ conversations: [...base.conversations, chosen] }))) setChosen('')
  }

  return (
    <section className="tt-links" aria-label={t('boards.links')}>
      <h3>{t('boards.documents')}</h3>
      {card.docs.length === 0 && <p className="tt-muted">{t('boards.noDocuments')}</p>}
      {card.docs.length > 0 && (
        <ul className="tt-link-list">
          {card.docs.map((p) => (
            <li key={p}>
              <code>{p}</code>
              {canWrite && (
                <button
                  type="button"
                  className="tt-button"
                  aria-label={t('boards.unlinkNamed', { name: p })}
                  disabled={busy}
                  onClick={() => onChange((base) => ({ docs: base.docs.filter((x) => x !== p) }))}
                >
                  {t('boards.unlink')}
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
      {canWrite && (
        <form className="tt-search" aria-label={t('boards.linkDocument')} onSubmit={linkDoc}>
          <input
            aria-label={t('boards.documentPath')}
            placeholder={t('boards.documentPlaceholder')}
            value={path}
            onChange={(e) => setPath(e.target.value)}
          />
          <button type="submit" className="tt-button" disabled={busy || !path.trim()}>
            {t('boards.link')}
          </button>
        </form>
      )}

      <h3>{t('boards.conversations')}</h3>
      {card.conversations.length === 0 && <p className="tt-muted">{t('boards.noConversations')}</p>}
      {card.conversations.length > 0 && (
        <ul className="tt-link-list">
          {card.conversations.map((id) => {
            const c = own(id)
            const title = c ? conversationTitle(c) : t('boards.conversation')
            return (
              <li key={id}>
                {c ? (
                  <Link href={routePath({ workspace, section: 'conversations', item: id })}>{title}</Link>
                ) : (
                  <span className="tt-muted">
                    {mine.data ? t('boards.othersConversation') : t('boards.conversationLoading')}
                  </span>
                )}
                {canWrite && (
                  <button
                    type="button"
                    className="tt-button"
                    aria-label={c ? t('boards.unlinkNamed', { name: title }) : t('boards.unlinkConversation')}
                    disabled={busy}
                    onClick={() => onChange((base) => ({ conversations: base.conversations.filter((x) => x !== id) }))}
                  >
                    {t('boards.unlink')}
                  </button>
                )}
              </li>
            )
          })}
        </ul>
      )}
      {canWrite && linkable.length > 0 && (
        <form className="tt-search" aria-label={t('boards.linkConversation')} onSubmit={linkConversation}>
          <select aria-label={t('boards.conversation')} value={chosen} onChange={(e) => setChosen(e.target.value)}>
            <option value="">{t('boards.chooseConversation')}</option>
            {linkable.map((c) => (
              <option key={c.id} value={String(c.id)}>
                {conversationTitle(c)}
              </option>
            ))}
          </select>
          <button type="submit" className="tt-button" disabled={busy || !chosen}>
            {t('boards.link')}
          </button>
        </form>
      )}
    </section>
  )
}

// A mention of someone in a comment (docs/api.md#mentions): the person's ID is what counts, and the
// name is only what is shown, so brackets, parentheses and line breaks can't be in it.
const mentionOf = (p: Person) => `@[${p.name.replace(/[[\]()\\\r\n]+/g, ' ').trim() || p.id}](user:${p.id})`

// Who a comment's mentions are, by ID, as the server names them to this reader. An ID the server
// gave no name for (someone this reader can't see, or no one) shows as `unknown`, so the label in
// the text, which anyone can write, is never what is shown.
const mentionNames = (c: Comment, unknown: string): MentionNames => ({
  names: new Map(c.mentions.map((m) => [String(m.id), m.name])),
  unknown,
})

function Thread({
  url,
  comments,
  canWrite,
  people,
  onAdded,
}: {
  url: string
  comments: Comment[]
  canWrite: boolean
  people: Person[] | undefined
  onAdded: (c: Comment) => void
}) {
  const [body, setBody] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const f = useFormat()
  const field = useRef<HTMLTextAreaElement>(null)
  // Puts the person's mention where the cursor is, and the cursor after it.
  const mention = (id: string) => {
    const who = people?.find((p) => String(p.id) === id)
    if (!who) return
    const at = field.current
    const start = at?.selectionStart ?? body.length
    const end = at?.selectionEnd ?? start
    const before = body.slice(0, start)
    const token = `${before && !/\s$/.test(before) ? ' ' : ''}${mentionOf(who)} `
    setBody(before + token + body.slice(end))
    const caret = before.length + token.length
    requestAnimationFrame(() => {
      at?.focus()
      at?.setSelectionRange(caret, caret)
    })
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      onAdded(await api<Comment>(url, { method: 'POST', body: { body } }))
      setBody('')
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <section className="tt-thread" aria-label={t('boards.comments')}>
      <h3>{t('boards.comments')}</h3>
      {comments.length === 0 && <p className="tt-muted">{t('boards.noComments')}</p>}
      <ol className="tt-list">
        {comments.map((c) => (
          <li key={c.id} className="tt-comment">
            <span className="tt-muted tt-small">
              {[authorLabel(t, c.author), f.dateTime(c.created_at)].join(' · ')}
            </span>
            <Markdown text={c.body} mentions={mentionNames(c, t('boards.mentionUnknown'))} />
          </li>
        ))}
      </ol>
      {canWrite && (
        <form className="tt-form" onSubmit={submit}>
          <textarea
            ref={field}
            aria-label={t('boards.commentField')}
            rows={3}
            value={body}
            onChange={(e) => setBody(e.target.value)}
          />
          {error && (
            <p role="alert" className="tt-error">
              {error}
            </p>
          )}
          <div className="tt-actions">
            {people && people.length > 0 && (
              <select
                aria-label={t('boards.mention')}
                value=""
                onChange={(e) => mention(e.target.value)}
                disabled={busy}
              >
                <option value="">{t('boards.mentionPlaceholder')}</option>
                {people.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
            )}
            <button type="submit" className="tt-button tt-primary" disabled={busy || !body.trim()}>
              {t('boards.commentSubmit')}
            </button>
          </div>
        </form>
      )}
    </section>
  )
}

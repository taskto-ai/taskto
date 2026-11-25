// Boards: a space's boards, the chosen board's cards by column, and the open card. Writers
// add, rename and archive boards, add and edit cards, assign them, link documents and
// conversations to them, move them between columns (with the Column field, or by dragging
// them), comment, mark them blocked, and give them a due date and labels (the board can be
// narrowed to a label, or to the overdue cards); viewers only read. Every change sends the version
// last read: if someone changed the card meanwhile, the server answers 409 with the card as
// it is, and the writer chooses to take that or to redo their change over it. The open
// card's thread is polled, so others' comments appear. On a phone it is the board or the
// open card.
//
// This file is the page that composes the parts: the head (BoardsHeader.tsx), the forms for a
// board (BoardForms.tsx), the board's columns (BoardView.tsx) and the open card (CardView.tsx).

import { useState } from 'react'
import { ApiError, api, type Board, errorText, ws } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { parseBoardItem } from '../router'
import { useSpaces } from '../spaces'
import { NewBoard, RenameBoard } from './BoardForms'
import { BoardBar, BoardsHeader } from './BoardsHeader'
import { BoardView } from './BoardView'

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
      <BoardsHeader
        spaces={spaces.data}
        space={space}
        onSpace={(id) => {
          setAdding(false)
          setRenaming(false)
          setBoardError(null)
          choose(id)
        }}
        boards={boards.data}
        board={board}
        onBoard={(id) => {
          setRenaming(false)
          setBoardError(null)
          setChosen(id)
        }}
        canWrite={canWrite}
        onAdd={() => setAdding(true)}
        archived={archived}
        onArchived={setArchived}
      />
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
        <BoardBar
          board={board}
          canWrite={canWrite}
          onRename={() => setRenaming(true)}
          onArchive={() => changeBoard(board, { archived: !board.archived_at })}
        />
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

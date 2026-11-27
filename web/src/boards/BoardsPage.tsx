import { type ReactNode, useState } from 'react'
import { ApiError, api, type Board, errorText, ws } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { parseBoardItem } from '../router'
import { useSpaces } from '../spaces'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { EmptyState } from '../ui/EmptyState'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { NewBoard, RenameBoard } from './BoardForms'
import { BoardMenu, BoardsHeader } from './BoardsHeader'
import { BoardSkeleton, BoardView } from './BoardView'
import './boards.css'

export function BoardsPage({ workspace, open }: { workspace: number; open?: string }) {
  const target = parseBoardItem(open)
  const { spaces, space, choose } = useSpaces(workspace, target?.space)
  const base = space ? `${ws(workspace)}/spaces/${space.id}/boards` : null
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
  const [confirmDialog, confirm] = useConfirm()
  const linkedMissing =
    boards.data !== undefined &&
    target?.board !== undefined &&
    chosen === target.board &&
    !boards.data.some((b) => b.id === chosen)
  const board = linkedMissing ? undefined : (boards.data?.find((b) => b.id === chosen) ?? boards.data?.[0])
  const canWrite = space?.access === 'write'
  const t = useT()

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
  const archiveBoard = async (b: Board) => {
    if (
      !b.archived_at &&
      !(await confirm({
        title: t('boards.archiveBoardTitle', { name: b.name }),
        body: t('boards.archiveBoardBody'),
        confirmLabel: t('boards.archiveBoard'),
      }))
    )
      return
    await changeBoard(b, { archived: !b.archived_at })
  }

  const menu = (
    <BoardMenu
      board={board}
      space={space}
      canWrite={canWrite}
      archived={archived}
      onRename={() => {
        setBoardError(null)
        setRenaming(true)
      }}
      onNew={() => setAdding(true)}
      onArchived={setArchived}
      onArchive={() => board && archiveBoard(board)}
    />
  )
  const header = (filters?: ReactNode, add?: ReactNode) => (
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
      filters={filters}
      menu={menu}
      add={add}
    />
  )

  const loadError = spaces.error ?? boards.error
  const loading =
    (spaces.data === undefined || base !== null) && boards.data === undefined && !boards.error && !spaces.error
  const notices = (
    <>
      {loadError && <Notice tone="danger">{loadError}</Notice>}
      {linkedMissing && (
        <Notice
          tone="info"
          action={
            !archived &&
            space?.kind === 'shared' && <Button onClick={() => setArchived(true)}>{t('boards.showArchived')}</Button>
          }
        >
          {t(archived ? 'boards.linkedUnavailable' : 'boards.linkedArchivedOrGone')}
        </Notice>
      )}
      {board?.archived_at && <Notice tone="neutral">{t('boards.archivedNotice')}</Notice>}
      {boardError && !renaming && <Notice tone="danger">{boardError}</Notice>}
    </>
  )

  return (
    <Page width="full">
      {base && space && board ? (
        <BoardView
          key={board.id}
          url={`${base}/${board.id}/cards`}
          workspace={workspace}
          space={space}
          canWrite={canWrite && !board.archived_at}
          openCard={board.id === target?.board ? target.card : undefined}
          header={header}
          notices={notices}
        />
      ) : (
        <>
          {header()}
          {notices}
          {spaces.data?.length === 0 && <EmptyState title={t('boards.noSpaces')} />}
          {loading && <BoardSkeleton />}
          {boards.data?.length === 0 && !linkedMissing && (
            <EmptyState
              title={t('boards.noBoards')}
              actions={
                canWrite &&
                space?.kind === 'shared' && (
                  <Button variant="primary" icon="plus" onClick={() => setAdding(true)}>
                    {t('boards.newBoard')}
                  </Button>
                )
              }
            >
              {canWrite && space?.kind === 'shared' ? t('boards.noBoardsWriter') : undefined}
            </EmptyState>
          )}
        </>
      )}
      {board && renaming && (
        <RenameBoard
          board={board}
          error={boardError}
          onSave={(name) => changeBoard(board, { name })}
          onCancel={() => {
            setRenaming(false)
            setBoardError(null)
          }}
        />
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
      {confirmDialog}
    </Page>
  )
}

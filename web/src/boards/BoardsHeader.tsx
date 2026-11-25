import type { Board, Space } from '../api'
import { useT } from '../i18n'
import { Icon } from '../shell/Icon'
import { SpacePicker } from '../spaces'

// The page's head: the space, the board among the space's, a new board, and whether archived
// boards are listed.
export function BoardsHeader({
  spaces,
  space,
  onSpace,
  boards,
  board,
  onBoard,
  canWrite,
  onAdd,
  archived,
  onArchived,
}: {
  spaces: Space[] | undefined
  space: Space | undefined
  onSpace: (id: number) => void
  boards: Board[] | undefined
  board: Board | undefined
  onBoard: (id: number) => void
  canWrite: boolean
  onAdd: () => void
  archived: boolean
  onArchived: (archived: boolean) => void
}) {
  const t = useT()
  return (
    <div className="tt-pane-head">
      <h1>{t('shell.sections.boards')}</h1>
      {spaces && <SpacePicker spaces={spaces} space={space} choose={onSpace} />}
      {boards && boards.length > 1 && (
        <label className="tt-inline-field">
          {t('boards.board')}
          <select value={board?.id ?? ''} onChange={(e) => onBoard(Number(e.target.value))}>
            {!board && (
              <option value="" disabled>
                {t('boards.board')}
              </option>
            )}
            {boards.map((b) => (
              <option key={b.id} value={b.id}>
                {b.archived_at ? t('boards.archivedBoard', { name: b.name }) : b.name}
              </option>
            ))}
          </select>
        </label>
      )}
      {/* A private space has only its personal board, which can't be archived. */}
      {canWrite && space?.kind === 'shared' && (
        <button type="button" className="tt-button" onClick={onAdd}>
          <Icon name="plus" />
          {t('boards.newBoard')}
        </button>
      )}
      {space?.kind === 'shared' && (
        <label className="tt-inline-field">
          <input type="checkbox" checked={archived} onChange={(e) => onArchived(e.target.checked)} />
          {t('boards.showArchived')}
        </label>
      )}
    </div>
  )
}

// The chosen board's name, and renaming and archiving it.
export function BoardBar({
  board,
  canWrite,
  onRename,
  onArchive,
}: {
  board: Board
  canWrite: boolean
  onRename: () => void
  onArchive: () => void
}) {
  const t = useT()
  return (
    <div className="tt-actions">
      <strong>{board.name}</strong>
      {board.archived_at && <span className="tt-muted">{t('boards.archivedNotice')}</span>}
      {canWrite && !board.archived_at && (
        <button type="button" className="tt-button" onClick={onRename}>
          <Icon name="edit" />
          {t('boards.rename')}
        </button>
      )}
      {canWrite && board.kind === 'team' && (
        <button type="button" className="tt-button" onClick={onArchive}>
          <Icon name="archive" />
          {board.archived_at ? t('boards.unarchiveBoard') : t('boards.archiveBoard')}
        </button>
      )}
    </div>
  )
}

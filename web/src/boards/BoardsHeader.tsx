import type { ReactNode } from 'react'
import type { Board, Space } from '../api'
import { useT } from '../i18n'
import { spaceLabel } from '../spaces'
import { Icon } from '../ui/Icon'
import { Menu, MenuButton, MenuItem, MenuList, MenuSeparator, MoreMenu } from '../ui/Menu'
import { PageHeader } from '../ui/PageHeader'

// The page's head: where it is (the space, a menu of the others when there are several, and
// "Boards"), the board's name as the title with a menu of the space's other boards, and the
// actions: the board's filters, the board's own menu and Add a card, the page's one primary
// action (the filters and Add a card come from the board).
export function BoardsHeader({
  spaces,
  space,
  onSpace,
  boards,
  board,
  onBoard,
  filters,
  menu,
  add,
}: {
  spaces: Space[] | undefined
  space: Space | undefined
  onSpace: (id: number) => void
  boards: Board[] | undefined
  board: Board | undefined
  onBoard: (id: number) => void
  filters?: ReactNode
  menu?: ReactNode
  add?: ReactNode
}) {
  const t = useT()
  const boardName = (b: Board) => (b.archived_at ? t('boards.archivedBoard', { name: b.name }) : b.name)
  return (
    <PageHeader
      path={
        <>
          {spaces && space && spaces.length > 1 ? (
            <Menu align="start">
              <MenuButton size="sm" label={t('knowledge.spaces.label')} className="ui-board-value-button">
                {spaceLabel(space)}
              </MenuButton>
              <MenuList>
                {spaces.map((s) => (
                  <MenuItem
                    key={s.id}
                    icon={s.id === space.id ? 'check' : undefined}
                    onSelect={() => s.id !== space.id && onSpace(s.id)}
                  >
                    {spaceLabel(s)}
                  </MenuItem>
                ))}
              </MenuList>
            </Menu>
          ) : (
            space && <span>{spaceLabel(space)}</span>
          )}
          {space && <Icon name="chevron-end" size={14} />}
          <span>{t('shell.sections.boards')}</span>
        </>
      }
      title={board?.name ?? t('shell.sections.boards')}
      titleAction={
        boards &&
        // Several boards, or one that isn't the one shown (a link named a board that is gone).
        (boards.length > 1 || (boards.length > 0 && !board)) && (
          <Menu align="start">
            <MenuButton label={t('boards.switchBoard')} icon="chevron-updown" />
            <MenuList>
              {boards.map((b) => (
                <MenuItem
                  key={b.id}
                  icon={b.id === board?.id ? 'check' : undefined}
                  onSelect={() => b.id !== board?.id && onBoard(b.id)}
                >
                  {boardName(b)}
                </MenuItem>
              ))}
            </MenuList>
          </Menu>
        )
      }
      actions={
        (filters || menu || add) && (
          <>
            {filters}
            {menu}
            {add}
          </>
        )
      }
    />
  )
}

// The board's own menu: renaming it, a new board, whether archived boards are listed, and
// archiving or unarchiving it. A private space has only its personal board, which can't be
// archived, and no other boards.
export function BoardMenu({
  board,
  space,
  canWrite,
  archived,
  onRename,
  onNew,
  onArchived,
  onArchive,
}: {
  board: Board | undefined
  space: Space | undefined
  canWrite: boolean
  archived: boolean
  onRename: () => void
  onNew: () => void
  onArchived: (archived: boolean) => void
  onArchive: () => void
}) {
  const t = useT()
  const shared = space?.kind === 'shared'
  const rename = canWrite && board && !board.archived_at
  const archive = canWrite && board?.kind === 'team'
  if (!rename && !(canWrite && shared) && !shared && !archive) return null
  return (
    <MoreMenu label={t('boards.boardActions')}>
      {rename && (
        <MenuItem icon="edit" onSelect={onRename}>
          {t('boards.rename')}
        </MenuItem>
      )}
      {canWrite && shared && (
        <MenuItem icon="plus" onSelect={onNew}>
          {t('boards.newBoard')}
        </MenuItem>
      )}
      {shared && (
        <MenuItem onSelect={() => onArchived(!archived)}>
          {archived ? t('boards.hideArchived') : t('boards.showArchived')}
        </MenuItem>
      )}
      {archive && board && (
        <>
          <MenuSeparator />
          <MenuItem icon="archive" onSelect={onArchive}>
            {board.archived_at ? t('boards.unarchiveBoard') : t('boards.archiveBoard')}
          </MenuItem>
        </>
      )}
    </MoreMenu>
  )
}

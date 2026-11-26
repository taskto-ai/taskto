import { type RefObject, useCallback, useEffect, useMemo } from 'react'
import type { Card, Space } from '../api'
import { useT } from '../i18n'
import { useBeforeLeave } from '../router'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Drawer } from '../ui/Drawer'
import { MenuItem, MoreMenu } from '../ui/Menu'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { Assignee } from './Assignee'
import { Blocked } from './Blocked'
import { CardDescription } from './CardDescription'
import { Prop, Status } from './CardMeta'
import { useCard } from './card'
import { DraftsContext, makeDrafts } from './drafts'
import { useFocusKeeper } from './focus'
import { Conversations, Documents } from './Links'
import { Due, Labels } from './Planning'
import { usePeople } from './people'
import { Composer, Thread } from './Thread'

// The open card, in a drawer over the board (on a phone, over the whole screen): its title, its
// properties as rows, each changed in place, its description, its thread and the box for a new
// comment, and archiving or restoring it in its menu. useCard in card.ts says how a change is
// sent, and what a conflict does. While it holds something unsaved (drafts.ts), closing it (Escape,
// its close button) or opening another card asks first; `leave` is that question, for the board.
export function CardView({
  url,
  workspace,
  space,
  board,
  canWrite,
  moved,
  onChanged,
  onClose,
  leave,
}: {
  url: string
  workspace: number
  space: Space
  board: Card[]
  canWrite: boolean
  moved: Card | null
  onChanged: () => void
  onClose: () => void
  // Set to the card's question before it goes: true when it may (nothing unsaved, or the person
  // chose to discard it).
  leave: RefObject<(() => Promise<boolean>) | null>
}) {
  const {
    card,
    loadError,
    busy,
    error,
    conflict,
    stopped,
    editing,
    setEditing,
    change,
    takeTheirs,
    addComment,
    patch,
    post,
  } = useCard(url, moved, onChanged)
  const people = usePeople(workspace, space, canWrite)
  const t = useT()
  useFocusKeeper('.ui-drawer')
  // A change can't start while one is being sent, or while a conflict waits for the writer.
  const held = busy || conflict !== null
  const drafts = useMemo(makeDrafts, [])
  const [confirmDialog, confirm] = useConfirm()
  const mayLeave = useCallback(
    async () =>
      !drafts.any() ||
      confirm({
        title: t('boards.discardTitle'),
        body: t('boards.discardBody'),
        confirmLabel: t('boards.discard'),
        tone: 'danger',
      }),
    [drafts, confirm, t],
  )
  useEffect(() => {
    leave.current = mayLeave
    return () => {
      leave.current = null
    }
  }, [leave, mayLeave])
  const close = async () => {
    if (await mayLeave()) onClose()
  }
  // Leaving the page (another section, another workspace, the browser's Back) asks the same, while
  // a description or a comment is being written.
  useBeforeLeave((go) => {
    void mayLeave().then((ok) => ok && go())
  })

  return (
    <DraftsContext.Provider value={drafts}>
      <Drawer
        open
        onClose={close}
        className="ui-board-drawer"
        title={card?.title ?? t('boards.card')}
        headerActions={
          canWrite &&
          card && (
            <MoreMenu label={t('boards.cardActions')}>
              {card.column === 'archived' ? (
                <MenuItem icon="archive" disabled={held} onSelect={() => change(post('restore'))}>
                  {t('boards.restore')}
                </MenuItem>
              ) : (
                <MenuItem icon="archive" disabled={held} onSelect={() => change(post('archive'))}>
                  {t('boards.archive')}
                </MenuItem>
              )}
            </MoreMenu>
          )
        }
        footer={canWrite && card && <Composer url={`${url}/comments`} people={people.data} onAdded={addComment} />}
      >
        {loadError && !card && <Notice tone="danger">{loadError}</Notice>}
        {!card && !loadError && <CardSkeleton />}
        {conflict && (
          <Notice
            tone="danger"
            action={
              <div className="ui-board-actions">
                <Button size="sm" onClick={() => takeTheirs(conflict.current)}>
                  {t('boards.takeTheirs')}
                </Button>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={busy}
                  onClick={() => change(conflict.send, conflict.current)}
                >
                  {t('boards.redoMine')}
                </Button>
              </div>
            }
          >
            {t('boards.cardChanged')}
          </Notice>
        )}
        {error && <Notice tone="danger">{error}</Notice>}
        {stopped && <Notice tone="danger">{t('boards.pollStopped', { error: stopped })}</Notice>}
        {card && (
          <>
            <dl className="ui-board-props">
              <Prop label={t('boards.status')}>
                <Status
                  card={card}
                  canWrite={canWrite}
                  busy={held}
                  onMove={(column) => change(post('move', { column }))}
                />
              </Prop>
              <Prop label={t('boards.assignee')}>
                <Assignee
                  card={card}
                  canWrite={canWrite}
                  people={people.data}
                  busy={held}
                  onSet={(assignee) => change(patch({ assignee }))}
                />
              </Prop>
              <Prop label={t('boards.dueDate')}>
                <Due
                  card={card}
                  canWrite={canWrite}
                  busy={held}
                  onChange={(fields) => change(patch(fields), card, true)}
                />
              </Prop>
              <Prop label={t('boards.labels')}>
                <Labels
                  card={card}
                  canWrite={canWrite}
                  busy={held}
                  onChange={(fields) => change(patch(fields), card, true)}
                />
              </Prop>
              <Prop label={t('boards.blocked')}>
                <Blocked
                  key={`blocked-${card.version}`}
                  card={card}
                  board={board}
                  canWrite={canWrite}
                  busy={held}
                  onSet={(reason, by) => change(patch({ blocked_reason: reason, blocked_by: by }))}
                />
              </Prop>
              <Prop label={t('boards.documents')}>
                <Documents card={card} canWrite={canWrite} busy={held} onChange={(fields) => change(patch(fields))} />
              </Prop>
              <Prop label={t('boards.conversations')}>
                <Conversations
                  workspace={workspace}
                  card={card}
                  canWrite={canWrite}
                  busy={held}
                  onChange={(fields) => change(patch(fields))}
                />
              </Prop>
            </dl>
            <CardDescription
              card={card}
              canWrite={canWrite}
              editing={editing}
              busy={held}
              onEdit={() => setEditing(true)}
              onSave={(fields) => change(patch(fields))}
              onCancel={() => setEditing(false)}
            />
            <Thread comments={card.comments} />
          </>
        )}
      </Drawer>
      {confirmDialog}
    </DraftsContext.Provider>
  )
}

// The card's shape while it loads: its rows, then its description.
function CardSkeleton() {
  const t = useT()
  return (
    <div className="ui-board-props" aria-busy="true">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      {[40, 55, 35, 50, 30].map((w) => (
        <div key={w} className="ui-board-prop">
          <Skeleton width="70%" />
          <Skeleton width={`${w}%`} />
        </div>
      ))}
    </div>
  )
}

import { appPath } from '../apps/route'
import { type T, useT } from '../i18n'
import { boardItem, documentItem, Link, routePath } from '../router'
import { Badge, type BadgeTone } from '../ui/Badge'
import { Icon, type IconName } from '../ui/Icon'
import { type Changed, type ChangeKind, changeKey } from './changes'

const TONES: Record<ChangeKind, BadgeTone> = {
  created: 'success',
  edited: 'info',
  moved: 'neutral',
  deleted: 'danger',
  restored: 'info',
  archived: 'neutral',
  commented: 'neutral',
}

const UNLINKED: ReadonlySet<ChangeKind> = new Set(['deleted', 'archived'])

type Row = { icon: IconName; name: string; folder?: string; href?: string }

function rowOf(t: T, workspace: number, c: Changed): Row {
  const href = UNLINKED.has(c.change) ? undefined : linkOf(workspace, c)
  switch (c.kind) {
    case 'doc': {
      const at = c.path.lastIndexOf('/')
      return {
        icon: 'file',
        name: c.path.slice(at + 1),
        ...(at > 0 ? { folder: c.path.slice(0, at) } : {}),
        href,
      }
    }
    case 'card':
      return { icon: 'boards', name: c.title || t('conversations.changes.card', { id: c.card }), href }
    case 'board':
      return { icon: 'boards', name: c.name, href }
    case 'app':
      return { icon: 'apps', name: c.name, href }
    case 'record':
      return {
        icon: 'apps',
        name: c.appName
          ? t('conversations.changes.record', { id: c.record, app: c.appName })
          : t('conversations.changes.recordBare', { id: c.record }),
        href: c.change === 'deleted' ? undefined : linkOf(workspace, c),
      }
  }
}

function linkOf(workspace: number, c: Changed): string {
  switch (c.kind) {
    case 'doc':
      return routePath({ workspace, section: 'knowledge', item: documentItem(c.space, c.path) })
    case 'card':
      return routePath({ workspace, section: 'boards', item: boardItem(c.space, c.board, c.card) })
    case 'board':
      return routePath({ workspace, section: 'boards', item: boardItem(c.space, c.board) })
    case 'app':
    case 'record':
      return appPath(workspace, c.space, c.app)
  }
}

export function TurnChanges({ changes, workspace }: { changes: Changed[]; workspace: number }) {
  const t = useT()
  return (
    <details className="ui-chat-changes" open>
      <summary className="ui-chat-changes-title">{t('conversations.changes.title', { count: changes.length })}</summary>
      <ul className="ui-chat-changes-list">
        {changes.map((c) => {
          const row = rowOf(t, workspace, c)
          return (
            <li key={changeKey(c)} className="ui-chat-changes-row">
              <Icon name={row.icon} size={14} className="ui-chat-changes-icon" />
              <span className="ui-chat-changes-name">
                {row.href ? (
                  <Link className="ui-chat-tool-link" href={row.href}>
                    {row.name}
                  </Link>
                ) : (
                  row.name
                )}
                {row.folder && <span className="ui-chat-changes-folder">{row.folder}</span>}
              </span>
              <Badge tone={TONES[c.change]}>{t(`conversations.changes.${c.change}`)}</Badge>
            </li>
          )
        })}
      </ul>
    </details>
  )
}

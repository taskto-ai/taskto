import { useT } from '../i18n'
import { IconButton } from '../ui/IconButton'
import '../ui/Tabs.css'
import type { Side } from './layout'

export type StripTab = { id: string; title: string }

export const tabDomId = (id: string) => `desk-tab-${id}`
export const panelDomId = (group: string) => `desk-panel-${group}`

export function TabStrip({
  group,
  tabs,
  active,
  closable,
  onActivate,
  onClose,
  onNew,
  onSplit,
}: {
  group: string
  tabs: StripTab[]
  active: string
  closable: boolean
  onActivate: (id: string) => void
  onClose: (id: string) => void
  onNew: () => void
  onSplit: (side: Side) => void
}) {
  const t = useT()
  return (
    <div className="desk-strip">
      <div className="desk-tabs" role="tablist" aria-label={t('desk.tabs')}>
        {tabs.map((tab) => {
          const selected = tab.id === active
          return (
            <div key={tab.id} className="desk-tab" role="presentation" data-selected={selected || undefined}>
              <button
                type="button"
                role="tab"
                id={tabDomId(tab.id)}
                aria-selected={selected}
                aria-controls={selected ? panelDomId(group) : undefined}
                className="ui-tab desk-tab-button"
                onClick={() => onActivate(tab.id)}
              >
                <span className="ui-tab-label">{tab.title}</span>
              </button>
              {closable && (
                <IconButton
                  icon="close"
                  size="sm"
                  label={t('desk.closeTab', { title: tab.title })}
                  className="desk-tab-close"
                  onClick={() => onClose(tab.id)}
                />
              )}
            </div>
          )
        })}
      </div>
      <div className="desk-strip-actions">
        <IconButton icon="plus" size="sm" label={t('desk.newTab')} onClick={onNew} />
        <IconButton icon="boards" size="sm" label={t('desk.splitInline')} onClick={() => onSplit('inline-end')} />
        <IconButton icon="menu" size="sm" label={t('desk.splitBlock')} onClick={() => onSplit('block-end')} />
      </div>
    </div>
  )
}

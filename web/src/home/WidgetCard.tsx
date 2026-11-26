// One widget on Home, in a panel of its own: its name, the app it comes from as a quiet line, what
// the app draws for it, and the one way to open the app. While Home is being customized, a widget of
// the person's own carries its controls (move earlier, move later, remove), and a space's default
// the one it has (hide). The controls show short or as icons; their names say which widget they act
// on, for whoever hears them.
//
// How the widget draws its own content is the app's renderer's (AppView): Home frames it.

import { useMemo } from 'react'
import { type HomeItem, ws } from '../api'
import { LazyAppView as AppView } from '../apps/LazyAppView'
import { appPath } from '../apps/route'
import { httpTransport } from '../apps/transport'
import { type Key, useT } from '../i18n'
import { Button, ButtonLink } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { IconButton } from '../ui/IconButton'
import { Panel } from '../ui/Panel'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'

export const itemKey = (i: HomeItem) => `${i.source}:${i.id ?? i.default_id}`

export function WidgetCard({
  workspace,
  item,
  customizing,
  onRemove,
  onHide,
  onMove,
  first,
  last,
}: {
  workspace: number
  item: HomeItem
  customizing: boolean
  onRemove: (name: string) => void
  onHide: (name: string) => void
  onMove?: (step: -1 | 1) => void
  first: boolean
  last: boolean
}) {
  const t = useT()
  const widget = item.widget
  const title = widget?.title ?? item.app_name
  const headingId = `ui-widget-${itemKey(item).replace(/\W/g, '-')}`
  const open = appPath(workspace, item.space_id, item.app_id)
  const ready = item.state === 'ready' && widget && item.release
  const focus = (control: string) => `widget:${itemKey(item)}:${control}`
  const base = `${ws(workspace)}/spaces/${item.space_id}/apps/${item.app_id}`
  const transport = useMemo(
    () => httpTransport(base, item.release?.number, { widget: item.widget_id }),
    [base, item.release?.number, item.widget_id],
  )
  return (
    <Panel
      role="article"
      aria-labelledby={headingId}
      className="ui-home-widget"
      data-state={ready ? 'ready' : 'unavailable'}
    >
      <div className="ui-home-widget-head">
        <div className="ui-home-widget-titles">
          <Heading level={2} size="subsection" id={headingId}>
            {title}
          </Heading>
          <p className="ui-home-widget-from">
            {item.source === 'default'
              ? t('home.fromAppDefault', { name: item.app_name })
              : t('home.fromApp', { name: item.app_name })}
          </p>
        </div>
        {customizing && (
          <div className="ui-home-widget-controls">
            {onMove && (
              <>
                <IconButton
                  size="sm"
                  icon="up"
                  label={t('home.moveEarlier', { name: title })}
                  data-home-focus={focus('earlier')}
                  disabled={first}
                  onClick={() => onMove(-1)}
                />
                <IconButton
                  size="sm"
                  icon="down"
                  label={t('home.moveLater', { name: title })}
                  data-home-focus={focus('later')}
                  disabled={last}
                  onClick={() => onMove(1)}
                />
              </>
            )}
            {item.source === 'personal' ? (
              <IconButton
                size="sm"
                icon="close"
                label={t('home.removeNamed', { name: title })}
                data-home-focus={focus('remove')}
                onClick={() => onRemove(title)}
              />
            ) : (
              <IconButton
                size="sm"
                icon="close"
                label={t('home.hideNamed', { name: title })}
                data-home-focus={focus('hide')}
                onClick={() => onHide(title)}
              />
            )}
          </div>
        )}
      </div>
      {ready ? (
        <>
          {widget.kind !== 'shortcut' && (
            <div className="ui-home-widget-body">
              <AppView
                key={`${item.release?.number}`}
                surfaceId={`app-${item.app_id}`}
                label={title}
                release={item.release}
                transport={transport}
                // Bare: the widget is its frame. The app keeps the room its rings and the primary's lift need.
                frame="bare"
              />
            </div>
          )}
          <div className="ui-home-widget-foot">
            <ButtonLink variant="ghost" size="sm" icon="chevron-end" href={open}>
              {t('home.openApp', { name: item.app_name })}
            </ButtonLink>
          </div>
        </>
      ) : (
        <p className="ui-home-widget-note" role="status">
          {t(`home.unavailable.${item.reason ?? 'removed'}` as Key, { name: item.app_name })}
        </p>
      )}
    </Panel>
  )
}

// In place of a widget that failed to draw: it says so, and a widget of the person's own can still
// be taken off Home. Nothing of the widget's answer is trusted here, not even its title.
export function WidgetFailed({ item, onRemove }: { item: HomeItem; onRemove?: () => void }) {
  const t = useT()
  return (
    <Panel className="ui-home-widget" data-state="failed">
      <p className="ui-home-widget-note" role="status">
        {t('home.failed')}
      </p>
      {onRemove && (
        <div className="ui-home-widget-foot">
          <Button size="sm" icon="close" onClick={onRemove}>
            {t('home.remove')}
          </Button>
        </div>
      )}
      <VisuallyHidden>{typeof item.widget_id === 'string' ? item.widget_id : ''}</VisuallyHidden>
    </Panel>
  )
}

// While Home loads for the first time: the shape of a row of widgets, and a word for screen readers.
export function WidgetSkeletons() {
  const t = useT()
  return (
    <div className="ui-home-widgets" aria-busy="true">
      <VisuallyHidden>
        <span role="status">{t('app.loading')}</span>
      </VisuallyHidden>
      {/* One row of widgets, about the height of what usually follows: a widget or two, or the empty
          state. The second is shown only where the grid has two columns. */}
      {[0, 1].map((n) => (
        <div key={n} className="ui-home-cell ui-home-cell-skeleton" data-size="small">
          <Panel className="ui-home-widget" aria-hidden="true">
            <div className="ui-home-widget-titles">
              <Skeleton width="45%" />
              <Skeleton width="30%" />
            </div>
            <Skeleton shape="block" height="4rem" />
          </Panel>
        </div>
      ))}
    </div>
  )
}

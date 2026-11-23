// A person's Home: the widgets they put there from their apps (and those a shared space sets for its
// members), each a bounded view of an app drawn by the same renderer as the app and through the
// app's own surface and actions, so what a widget shows and does is what its viewer may see and do in
// the app, no more. Each widget is drawn inside an error boundary of its own, so one that fails (a
// malformed answer, a render error, an app this client is too old to draw) leaves the others and the
// shell as they were. An app that went offline, was unpublished or removed the widget says so, with
// no data; one the person lost access to isn't there at all.
//
// "Customize" is where people choose: which widgets to show and in what order, which apps to pin to
// the navigation, the defaults they hid, and, for those who may publish an app of a shared space, the
// widgets and pin it offers to all its members by default.

import { useMemo, useState } from 'react'
import { type AvailableApp, api, errorText, type HomeItem, type HomeView, ws } from '../api'
import { AppView, httpTransport } from '../apps/AppView'
import { appPath } from '../apps/route'
import { type Key, useT } from '../i18n'
import { useLoad } from '../load'
import { Link } from '../router'
import { ErrorBoundary } from '../shell/ErrorBoundary'
import { Icon } from '../shell/Icon'
import { homeChanged, useHomeVersion } from './store'

const itemKey = (i: HomeItem) => `${i.source}:${i.id ?? i.default_id}`

export function HomePage({ workspace }: { workspace: number }) {
  const t = useT()
  const version = useHomeVersion()
  const home = useLoad(() => api<HomeView>(`${ws(workspace)}/home`), [workspace, version])
  const [customizing, setCustomizing] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Runs a change, then tells everything that shows Home to load it again, whether it worked or not:
  // after a refusal the lists show what is true now.
  const run = async (change: () => Promise<unknown>) => {
    try {
      await change()
      setError(null)
    } catch (e) {
      setError(errorText(e))
    }
    homeChanged()
  }
  const base = ws(workspace)
  const remove = (item: HomeItem) => run(() => api(`${base}/home/items/${item.id}`, { method: 'DELETE' }))
  const hide = (item: HomeItem) => run(() => api(`${base}/home/hidden/${item.default_id}`, { method: 'PUT' }))
  // Moves one of the person's own placements among their own, one step.
  const move = (kind: 'widget' | 'pin', item: HomeItem, step: -1 | 1) => {
    const own = (kind === 'widget' ? (home.data?.widgets ?? []) : (home.data?.pins ?? [])).filter(
      (i) => i.source === 'personal',
    )
    const from = own.findIndex((i) => i.id === item.id)
    const to = from + step
    if (from < 0 || to < 0 || to >= own.length) return Promise.resolve()
    const ids = own.map((i) => i.id)
    ;[ids[from], ids[to]] = [ids[to], ids[from]]
    return run(() => api(`${base}/home/order`, { method: 'PUT', body: { kind, ids } }))
  }

  const widgets = home.data?.widgets ?? []
  return (
    <div className="tt-section tt-home">
      <div className="tt-pane-head">
        <h1>{t('shell.sections.home')}</h1>
        <button
          type="button"
          className="tt-button"
          aria-expanded={customizing}
          aria-controls="tt-home-customize"
          onClick={() => setCustomizing((c) => !c)}
        >
          {customizing ? t('home.done') : t('home.customize')}
        </button>
      </div>
      <p className="tt-muted">{t('home.intro')}</p>
      {(home.error || error) && (
        <p role="alert" className="tt-error">
          {error ?? home.error}
        </p>
      )}
      {!home.data && !home.error && <p className="tt-muted">{t('app.loading')}</p>}
      {home.data && widgets.length === 0 && <p className="tt-muted">{t('home.empty')}</p>}
      {widgets.length > 0 && (
        <ul className="tt-widgets" aria-label={t('home.widgets')}>
          {widgets.map((item) => {
            const own = widgets.filter((i) => i.source === 'personal')
            const at = own.findIndex((i) => i.id === item.id)
            return (
              <li key={itemKey(item)} className="tt-widget-cell" data-size={sizeOf(item)}>
                <ErrorBoundary
                  resetKey={`${version}:${itemKey(item)}`}
                  fallback={<WidgetFailed item={item} onRemove={item.id ? () => remove(item) : undefined} />}
                >
                  <WidgetCard
                    workspace={workspace}
                    item={item}
                    customizing={customizing}
                    onRemove={() => remove(item)}
                    onHide={() => hide(item)}
                    onMove={item.source === 'personal' ? (step) => move('widget', item, step) : undefined}
                    first={at <= 0}
                    last={at === own.length - 1}
                  />
                </ErrorBoundary>
              </li>
            )
          })}
        </ul>
      )}
      {customizing && home.data && (
        <div id="tt-home-customize">
          <Customize workspace={workspace} home={home.data} run={run} move={move} remove={remove} hide={hide} />
        </div>
      )}
    </div>
  )
}

// The row a widget takes: what its app says, if it says one of the sizes we know.
function sizeOf(item: HomeItem): string {
  const size = item.widget?.size
  return size === 'medium' || size === 'large' ? size : 'small'
}

function WidgetFailed({ item, onRemove }: { item: HomeItem; onRemove?: () => void }) {
  const t = useT()
  return (
    <article className="tt-widget" data-state="failed">
      <p role="status">{t('home.failed')}</p>
      {onRemove && (
        <button type="button" className="tt-button" onClick={onRemove}>
          {t('home.remove')}
        </button>
      )}
      <span className="tt-sr-only">{typeof item.widget_id === 'string' ? item.widget_id : ''}</span>
    </article>
  )
}

function WidgetCard({
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
  onRemove: () => void
  onHide: () => void
  onMove?: (step: -1 | 1) => void
  first: boolean
  last: boolean
}) {
  const t = useT()
  const widget = item.widget
  const title = widget?.title ?? item.app_name
  const headingId = `tt-widget-${itemKey(item).replace(/\W/g, '-')}`
  const open = appPath(workspace, item.space_id, item.app_id)
  const ready = item.state === 'ready' && widget && item.release
  const base = `${ws(workspace)}/spaces/${item.space_id}/apps/${item.app_id}`
  const transport = useMemo(
    () => httpTransport(base, item.release?.number, { widget: item.widget_id }),
    [base, item.release?.number, item.widget_id],
  )
  return (
    <article className="tt-widget" aria-labelledby={headingId} data-state={item.state}>
      <header className="tt-widget-head">
        <div className="tt-widget-title">
          <h2 id={headingId}>{title}</h2>
          <span className="tt-muted tt-small">
            {item.source === 'default'
              ? t('home.fromAppDefault', { name: item.app_name })
              : t('home.fromApp', { name: item.app_name })}
          </span>
        </div>
        {customizing && (
          <div className="tt-widget-controls">
            {onMove && (
              <>
                <button
                  type="button"
                  className="tt-icon-button"
                  aria-label={t('home.moveEarlier', { name: title })}
                  disabled={first}
                  onClick={() => onMove(-1)}
                >
                  <Icon name="up" />
                </button>
                <button
                  type="button"
                  className="tt-icon-button"
                  aria-label={t('home.moveLater', { name: title })}
                  disabled={last}
                  onClick={() => onMove(1)}
                >
                  <Icon name="down" />
                </button>
              </>
            )}
            {item.source === 'personal' ? (
              <button type="button" className="tt-button" onClick={onRemove}>
                {t('home.removeNamed', { name: title })}
              </button>
            ) : (
              <button type="button" className="tt-button" onClick={onHide}>
                {t('home.hideNamed', { name: title })}
              </button>
            )}
          </div>
        )}
      </header>
      {ready ? (
        <>
          {widget.kind !== 'shortcut' && (
            <div className="tt-widget-body">
              <AppView
                key={`${item.release?.number}`}
                surfaceId={`app-${item.app_id}`}
                label={title}
                release={item.release}
                transport={transport}
              />
            </div>
          )}
          <Link className="tt-widget-open" href={open}>
            {t('home.openApp', { name: item.app_name })}
          </Link>
        </>
      ) : (
        <p className="tt-muted" role="status">
          {t(`home.unavailable.${item.reason ?? 'removed'}` as Key, { name: item.app_name })}
        </p>
      )}
    </article>
  )
}

function Customize({
  workspace,
  home,
  run,
  move,
  remove,
  hide,
}: {
  workspace: number
  home: HomeView
  run: (change: () => Promise<unknown>) => Promise<void>
  move: (kind: 'widget' | 'pin', item: HomeItem, step: -1 | 1) => Promise<void>
  remove: (item: HomeItem) => Promise<void>
  hide: (item: HomeItem) => Promise<void>
}) {
  const t = useT()
  const version = useHomeVersion()
  const base = ws(workspace)
  const available = useLoad(
    () => api<{ apps: AvailableApp[] }>(`${base}/home/available`).then((r) => r.apps),
    [base, version],
  )
  const ownPins = home.pins.filter((p) => p.source === 'personal')
  const place = (a: AvailableApp, kind: 'widget' | 'pin', widget?: string) =>
    run(() =>
      api(`${base}/home/items`, { method: 'POST', body: { kind, space_id: a.space_id, app_id: a.app_id, widget } }),
    )
  const setDefault = (a: AvailableApp, kind: 'widget' | 'pin', widget?: string) =>
    run(() =>
      api(`${base}/spaces/${a.space_id}/home/defaults`, { method: 'POST', body: { kind, app_id: a.app_id, widget } }),
    )
  const unsetDefault = (a: AvailableApp, id: number) =>
    run(() => api(`${base}/spaces/${a.space_id}/home/defaults/${id}`, { method: 'DELETE' }))
  const space = (a: AvailableApp) => (a.space_kind === 'private' ? t('knowledge.spaces.private') : a.space_name)
  const placedWidget = (a: AvailableApp, id: string) =>
    home.widgets.find((i) => i.source === 'personal' && i.app_id === a.app_id && i.widget_id === id)
  const placedPin = (a: AvailableApp) => home.pins.find((i) => i.source === 'personal' && i.app_id === a.app_id)

  return (
    <div className="tt-home-customize">
      <section aria-labelledby="tt-home-pins">
        <h2 id="tt-home-pins">{t('home.pins')}</h2>
        <p className="tt-muted tt-small">{t('home.pinsIntro')}</p>
        {home.pins.length === 0 && <p className="tt-muted">{t('home.noPins')}</p>}
        <ul className="tt-home-list">
          {home.pins.map((p) => {
            const name = p.label || p.app_name
            const at = ownPins.findIndex((i) => i.id === p.id)
            return (
              <li key={itemKey(p)}>
                <span className="tt-home-name">
                  {name}
                  {p.state === 'unavailable' && (
                    <span className="tt-muted tt-small">
                      {' '}
                      {t(`home.unavailableShort.${p.reason ?? 'removed'}` as Key)}
                    </span>
                  )}
                  {p.source === 'default' && <span className="tt-muted tt-small"> {t('home.default')}</span>}
                </span>
                {p.source === 'personal' ? (
                  <>
                    <button
                      type="button"
                      className="tt-icon-button"
                      aria-label={t('home.moveEarlier', { name })}
                      disabled={at <= 0}
                      onClick={() => move('pin', p, -1)}
                    >
                      <Icon name="up" />
                    </button>
                    <button
                      type="button"
                      className="tt-icon-button"
                      aria-label={t('home.moveLater', { name })}
                      disabled={at === ownPins.length - 1}
                      onClick={() => move('pin', p, 1)}
                    >
                      <Icon name="down" />
                    </button>
                    <button type="button" className="tt-button" onClick={() => remove(p)}>
                      {t('home.removePinNamed', { name })}
                    </button>
                  </>
                ) : (
                  <button type="button" className="tt-button" onClick={() => hide(p)}>
                    {t('home.hideNamed', { name })}
                  </button>
                )}
              </li>
            )
          })}
        </ul>
      </section>

      {home.hidden.length > 0 && (
        <section aria-labelledby="tt-home-hidden">
          <h2 id="tt-home-hidden">{t('home.hidden')}</h2>
          <ul className="tt-home-list">
            {home.hidden.map((h) => {
              const name = h.widget?.title || h.label || h.app_name
              return (
                <li key={itemKey(h)}>
                  <span className="tt-home-name">{name}</span>
                  <button
                    type="button"
                    className="tt-button"
                    onClick={() => run(() => api(`${base}/home/hidden/${h.default_id}`, { method: 'DELETE' }))}
                  >
                    {t('home.showNamed', { name })}
                  </button>
                </li>
              )
            })}
          </ul>
        </section>
      )}

      <section aria-labelledby="tt-home-add">
        <h2 id="tt-home-add">{t('home.add')}</h2>
        {available.error && (
          <p role="alert" className="tt-error">
            {available.error}
          </p>
        )}
        {available.data?.length === 0 && <p className="tt-muted">{t('home.noApps')}</p>}
        <ul className="tt-home-apps">
          {available.data?.map((a) => {
            const pin = placedPin(a)
            const pinDefault = a.defaults.find((d) => d.kind === 'pin')
            const pinName = a.pin?.label || a.name
            return (
              <li key={a.app_id}>
                <h3>
                  {a.name} <span className="tt-muted tt-small">{space(a)}</span>
                </h3>
                <ul className="tt-home-list">
                  {a.widgets.map((w) => {
                    const placed = placedWidget(a, w.id)
                    const def = a.defaults.find((d) => d.kind === 'widget' && d.widget === w.id)
                    return (
                      <li key={w.id}>
                        <span className="tt-home-name">
                          {w.title} <span className="tt-muted tt-small">{t(`home.kind.${w.kind}` as Key)}</span>
                        </span>
                        {placed ? (
                          <button type="button" className="tt-button" onClick={() => remove(placed)}>
                            {t('home.removeNamed', { name: w.title })}
                          </button>
                        ) : (
                          <button type="button" className="tt-button" onClick={() => place(a, 'widget', w.id)}>
                            {t('home.addNamed', { name: w.title })}
                          </button>
                        )}
                        {a.can_set_defaults &&
                          (def ? (
                            <button type="button" className="tt-button" onClick={() => unsetDefault(a, def.id)}>
                              {t('home.unsetDefaultNamed', { name: w.title })}
                            </button>
                          ) : (
                            <button type="button" className="tt-button" onClick={() => setDefault(a, 'widget', w.id)}>
                              {t('home.setDefaultNamed', { name: w.title })}
                            </button>
                          ))}
                      </li>
                    )
                  })}
                  <li>
                    <span className="tt-home-name">
                      {pinName} <span className="tt-muted tt-small">{t('home.kind.pin')}</span>
                    </span>
                    {pin ? (
                      <button type="button" className="tt-button" onClick={() => remove(pin)}>
                        {t('home.unpinNamed', { name: pinName })}
                      </button>
                    ) : (
                      <button type="button" className="tt-button" onClick={() => place(a, 'pin')}>
                        {t('home.pinNamed', { name: pinName })}
                      </button>
                    )}
                    {a.can_set_defaults &&
                      (pinDefault ? (
                        <button type="button" className="tt-button" onClick={() => unsetDefault(a, pinDefault.id)}>
                          {t('home.unsetDefaultNamed', { name: pinName })}
                        </button>
                      ) : (
                        <button type="button" className="tt-button" onClick={() => setDefault(a, 'pin')}>
                          {t('home.setDefaultNamed', { name: pinName })}
                        </button>
                      ))}
                  </li>
                </ul>
              </li>
            )
          })}
        </ul>
        {available.data?.some((a) => a.can_set_defaults) && (
          <p className="tt-muted tt-small">{t('home.defaultsNote')}</p>
        )}
      </section>
    </div>
  )
}

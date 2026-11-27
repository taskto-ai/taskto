import { useCallback, useLayoutEffect, useRef, useState } from 'react'
import { type AvailableApp, api, errorText, type HomeItem, type HomeView, ws } from '../api'
import { useFormat, useT } from '../i18n'
import { routePath } from '../router'
import { sameSession } from '../session'
import { ErrorBoundary } from '../shell/ErrorBoundary'
import { Button, ButtonLink } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Icon } from '../ui/Icon'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { HomeCustomize } from './HomeCustomize'
import { SetupChecklist } from './SetupChecklist'
import { homeChanged, homeVersion, useHomeLoad, useHomeVersion } from './store'
import { itemKey, WidgetCard, WidgetFailed, WidgetSkeletons } from './WidgetCard'
import './Home.css'

export type Move = (kind: 'widget' | 'pin', item: HomeItem, step: -1 | 1) => Promise<void>
export type After = { message?: string; focus?: string[] }
export type Run = (change: () => Promise<unknown>, after?: After) => Promise<void>
export type Act = (item: HomeItem, name: string, focus?: string[]) => Promise<void>

export const focusName = (scope: string, key: string, control: string) => `${scope}:${key}:${control}`
function focusTarget(name: string): HTMLElement | undefined {
  return [...document.querySelectorAll<HTMLElement>('[data-home-focus]')].find((el) => el.dataset.homeFocus === name)
}

export function neighbours(scope: string, list: HomeItem[], item: HomeItem): string[] {
  const at = list.findIndex((i) => itemKey(i) === itemKey(item))
  return [list[at + 1], list[at - 1]]
    .filter((i): i is HomeItem => !!i)
    .flatMap((i) => ['remove', 'hide'].map((c) => focusName(scope, itemKey(i), c)))
}

export function HomePage({ workspace, admin = false }: { workspace: number; admin?: boolean }) {
  const t = useT()
  const f = useFormat()
  const heading = useRef<HTMLHeadingElement>(null)
  const customize = useRef<HTMLButtonElement>(null)
  const version = useHomeVersion()
  const home = useHomeLoad(() => {
    const at = homeVersion()
    return api<HomeView>(`${ws(workspace)}/home`).then((view) => ({ view, at }))
  }, [workspace])
  const view = home.data?.view
  const [customizing, setCustomizing] = useState(false)
  const [openedFromEmpty, setOpenedFromEmpty] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [said, setSaid] = useState('')
  const pendingFocus = useRef<{ names: string[]; at: number } | null>(null)

  const settleFocus = useCallback(() => {
    const pending = pendingFocus.current
    if (!pending || (home.data?.at ?? -1) < pending.at) return
    pendingFocus.current = null
    const names = pending.names
    for (const name of names) {
      const el = focusTarget(name)
      if (el && !(el as HTMLButtonElement).disabled) {
        el.focus()
        return
      }
    }
    customize.current?.focus()
  }, [home.data])
  useLayoutEffect(settleFocus, [settleFocus])

  const run: Run = async (change, after = {}) => {
    pendingFocus.current = null
    const same = sameSession()
    try {
      await change()
      if (!same()) return
      setError(null)
      if (after.message) setSaid(after.message)
    } catch (e) {
      if (!same()) return
      setError(errorText(e))
    }
    homeChanged()
    if (after.focus) pendingFocus.current = { names: after.focus, at: homeVersion() }
  }
  const base = ws(workspace)
  const scopeOf = (item: HomeItem) => (item.kind === 'pin' ? 'pin' : 'widget')
  const listOf = (item: HomeItem) => (item.kind === 'pin' ? (view?.pins ?? []) : (view?.widgets ?? []))
  const remove: Act = (item, name, focus) =>
    run(() => api(`${base}/home/items/${item.id}`, { method: 'DELETE' }), {
      message: t(item.kind === 'pin' ? 'home.announce.unpinned' : 'home.announce.removed', { name }),
      focus: focus ?? neighbours(scopeOf(item), listOf(item), item),
    })
  const hide: Act = (item, name, focus) =>
    run(() => api(`${base}/home/hidden/${item.default_id}`, { method: 'PUT' }), {
      message: t('home.announce.hidden', { name }),
      focus: focus ?? neighbours(scopeOf(item), listOf(item), item),
    })
  const move: Move = (kind, item, step) => {
    const own = (kind === 'widget' ? (view?.widgets ?? []) : (view?.pins ?? [])).filter((i) => i.source === 'personal')
    const from = own.findIndex((i) => i.id === item.id)
    const to = from + step
    if (from < 0 || to < 0 || to >= own.length) return Promise.resolve()
    const ids = own.map((i) => i.id)
    ;[ids[from], ids[to]] = [ids[to], ids[from]]
    const name = kind === 'widget' ? (item.widget?.title ?? item.app_name) : item.label || item.app_name
    const key = itemKey(item)
    const [same, other] = step < 0 ? ['earlier', 'later'] : ['later', 'earlier']
    return run(() => api(`${base}/home/order`, { method: 'PUT', body: { kind, ids } }), {
      message: t('home.announce.moved', { name, position: f.number(to + 1), total: f.number(own.length) }),
      focus: [focusName(kind, key, same), focusName(kind, key, other)],
    })
  }

  const widgets = view?.widgets ?? []
  const own = widgets.filter((i) => i.source === 'personal')
  const problem = error ?? home.error
  return (
    <Page width="list">
      <PageHeader
        title={t('shell.sections.home')}
        titleRef={heading}
        actions={
          <Button
            ref={customize}
            icon={customizing ? 'check' : 'edit'}
            aria-expanded={customizing}
            aria-controls="ui-home-customize"
            onClick={() => {
              setOpenedFromEmpty(false)
              setCustomizing((c) => !c)
            }}
          >
            {customizing ? t('home.done') : t('home.customize')}
          </Button>
        }
      />
      <VisuallyHidden>
        <span role="status">{said}</span>
      </VisuallyHidden>
      {admin && (
        <SetupChecklist
          workspace={workspace}
          onDismissed={() => heading.current?.focus()}
          onCompleted={() => heading.current?.focus()}
        />
      )}
      {problem && <Notice tone="danger">{problem}</Notice>}
      {!home.data && !home.error && <WidgetSkeletons />}
      {home.data && widgets.length === 0 && !customizing && (
        <EmptyHome
          workspace={workspace}
          onCustomize={() => {
            setOpenedFromEmpty(true)
            setCustomizing(true)
          }}
        />
      )}
      {widgets.length > 0 && (
        <ul className="ui-home-widgets" aria-label={t('home.widgets')}>
          {widgets.map((item) => {
            const at = own.findIndex((i) => i.id === item.id)
            return (
              <li key={itemKey(item)} className="ui-home-cell" data-size={sizeOf(item)}>
                <ErrorBoundary
                  resetKey={`${version}:${itemKey(item)}`}
                  fallback={
                    <WidgetFailed item={item} onRemove={item.id ? () => remove(item, item.app_name) : undefined} />
                  }
                >
                  <WidgetCard
                    workspace={workspace}
                    item={item}
                    customizing={customizing}
                    onRemove={(name) => remove(item, name)}
                    onHide={(name) => hide(item, name)}
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
      {customizing && view && (
        <HomeCustomize
          id="ui-home-customize"
          workspace={workspace}
          admin={admin}
          home={view}
          focusOnOpen={openedFromEmpty}
          run={run}
          move={move}
          remove={remove}
          hide={hide}
        />
      )}
    </Page>
  )
}

function sizeOf(item: HomeItem): string {
  const size = item.widget?.size
  return size === 'medium' || size === 'large' ? size : 'small'
}

function EmptyHome({ workspace, onCustomize }: { workspace: number; onCustomize: () => void }) {
  const t = useT()
  const available = useHomeLoad(
    () => api<{ apps: AvailableApp[] }>(`${ws(workspace)}/home/available`).then((r) => r.apps),
    [workspace],
  )
  const offers = available.data?.some((a) => a.widgets.length > 0)
  const settled = available.data !== undefined || available.error !== null
  return (
    <EmptyState
      icon={<Icon name="home" size={24} />}
      title={t('home.emptyTitle')}
      actions={
        settled &&
        (offers ? (
          <Button icon="plus" onClick={onCustomize}>
            {t('home.emptyAdd')}
          </Button>
        ) : (
          <ButtonLink icon="apps" href={routePath({ workspace, section: 'apps' })}>
            {t('home.emptyApps')}
          </ButtonLink>
        ))
      }
    >
      <p>{t(offers ? 'home.emptyHelp' : 'home.emptyNoApps')}</p>
    </EmptyState>
  )
}

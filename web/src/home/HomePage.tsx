// A person's Home: the widgets they put there from their apps (and those a shared space sets for its
// members), each a bounded view of an app drawn by the same renderer as the app and through the
// app's own surface and actions, so what a widget shows and does is what its viewer may see and do in
// the app, no more. Each widget is drawn inside an error boundary of its own, so one that fails (a
// malformed answer, a render error, an app this client is too old to draw) leaves the others and the
// shell as they were. An app that went offline, was unpublished or removed the widget says so, with
// no data; one the person lost access to isn't there at all.
//
// A workspace's admins also see its setup checklist above the widgets until it is done or they put it
// away (SetupChecklist.tsx).
//
// "Customize" is where people choose (HomeCustomize.tsx): which widgets to show and in what order,
// which apps to pin to the navigation, the defaults they hid, and, for those who may publish an app
// of a shared space, the widgets and pin it offers to all its members by default. While it is on,
// each widget of the person's own carries its controls: move earlier, move later, remove.

import { useCallback, useLayoutEffect, useRef, useState } from 'react'
import { type AvailableApp, api, errorText, type HomeItem, type HomeView, ws } from '../api'
import { useFormat, useT } from '../i18n'
import { routePath } from '../router'
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
import { homeChanged, useHomeLoad, useHomeVersion } from './store'
import { itemKey, WidgetCard, WidgetFailed, WidgetSkeletons } from './WidgetCard'
import './Home.css'

export type Move = (kind: 'widget' | 'pin', item: HomeItem, step: -1 | 1) => Promise<void>
// What follows a change: the words a screen reader is told once it is made, and where the keyboard
// goes once Home is shown again (the first of these controls that is there and enabled; the
// Customize button when none is).
export type After = { message?: string; focus?: string[] }
export type Run = (change: () => Promise<unknown>, after?: After) => Promise<void>
export type Act = (item: HomeItem, name: string, focus?: string[]) => Promise<void>

// The name a control carries so the keyboard can be put back on it after Home is shown again:
// what it acts on (a widget, a pin, an offer) and which control it is.
export const focusName = (scope: string, key: string, control: string) => `${scope}:${key}:${control}`
// The control that carries a focus name, found by comparing the names, not by building a selector
// from one (a name holds an item's key, which no selector should have to escape).
function focusTarget(name: string): HTMLElement | undefined {
  return [...document.querySelectorAll<HTMLElement>('[data-home-focus]')].find((el) => el.dataset.homeFocus === name)
}

// Where the keyboard goes when a placement is taken off: the same control of the next one, or the
// one before, of the same list.
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
  const home = useHomeLoad(() => api<HomeView>(`${ws(workspace)}/home`), [workspace])
  const [customizing, setCustomizing] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [said, setSaid] = useState('')
  const pendingFocus = useRef<string[] | null>(null)

  // Once Home is shown again after a change, the keyboard goes back where the change left it.
  const settleFocus = useCallback(() => {
    const names = pendingFocus.current
    if (!names) return
    pendingFocus.current = null
    for (const name of names) {
      const el = focusTarget(name)
      if (el && !(el as HTMLButtonElement).disabled) {
        el.focus()
        return
      }
    }
    customize.current?.focus()
  }, [])
  // biome-ignore lint/correctness/useExhaustiveDependencies: Home shown again is when to settle
  useLayoutEffect(settleFocus, [home.data, settleFocus])

  // Runs a change, then tells everything that shows Home to load it again, whether it worked or not:
  // after a refusal the lists show what is true now. What is shown stays while it loads.
  const run: Run = async (change, after = {}) => {
    pendingFocus.current = after.focus ?? null
    try {
      await change()
      setError(null)
      if (after.message) setSaid(after.message)
    } catch (e) {
      setError(errorText(e))
    }
    homeChanged()
  }
  const base = ws(workspace)
  const scopeOf = (item: HomeItem) => (item.kind === 'pin' ? 'pin' : 'widget')
  const listOf = (item: HomeItem) => (item.kind === 'pin' ? (home.data?.pins ?? []) : (home.data?.widgets ?? []))
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
  // Moves one of the person's own placements among their own, one step.
  const move: Move = (kind, item, step) => {
    const own = (kind === 'widget' ? (home.data?.widgets ?? []) : (home.data?.pins ?? [])).filter(
      (i) => i.source === 'personal',
    )
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
      // The same button, if the item can still go that way; the other one otherwise.
      focus: [focusName(kind, key, same), focusName(kind, key, other)],
    })
  }

  const widgets = home.data?.widgets ?? []
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
            onClick={() => setCustomizing((c) => !c)}
          >
            {customizing ? t('home.done') : t('home.customize')}
          </Button>
        }
      />
      {/* What a change in customize mode did, said once it is made. */}
      <VisuallyHidden>
        <span role="status">{said}</span>
      </VisuallyHidden>
      {/* Where the keyboard goes when the checklist, which had it, is put away or finished. */}
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
        <EmptyHome workspace={workspace} onCustomize={() => setCustomizing(true)} />
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
      {customizing && home.data && (
        <HomeCustomize
          id="ui-home-customize"
          workspace={workspace}
          admin={admin}
          home={home.data}
          run={run}
          move={move}
          remove={remove}
          hide={hide}
        />
      )}
    </Page>
  )
}

// The columns a widget takes on a wide screen: what its app says, if it says one of the sizes we know.
function sizeOf(item: HomeItem): string {
  const size = item.widget?.size
  return size === 'medium' || size === 'large' ? size : 'small'
}

// Nothing placed yet: what Home is for, and the first step. Customize when one of the person's apps
// offers a widget; the apps otherwise, where one is built or installed.
function EmptyHome({ workspace, onCustomize }: { workspace: number; onCustomize: () => void }) {
  const t = useT()
  const available = useHomeLoad(
    () => api<{ apps: AvailableApp[] }>(`${ws(workspace)}/home/available`).then((r) => r.apps),
    [workspace],
  )
  const offers = available.data?.some((a) => a.widgets.length > 0)
  // Still asking: no step yet, rather than one that changes under the pointer.
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

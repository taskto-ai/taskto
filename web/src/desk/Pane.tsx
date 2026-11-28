import { type ReactNode, useMemo } from 'react'
import type { Me, Workspace } from '../api'
import { useT } from '../i18n'
import { askPlace, beforeLeaving, navigate, type Place, PlaceContext, parseRoute } from '../router'
import { ErrorBoundary } from '../shell/ErrorBoundary'
import { SECTION_LABEL } from '../shell/nav'
import { SectionPage } from '../shell/SectionPage'
import {
  activate,
  type Change,
  closeTab,
  focusGroup,
  type GroupNode,
  type Layout,
  navigateTab,
  openTab,
  type Side,
  splitGroup,
} from './layout'
import { panelDomId, TabStrip, tabDomId } from './TabStrip'

export type Titles = Record<string, { path: string; title: string }>

export type ChangeFn = (fn: (l: Layout) => Change) => unknown

export function Pane({
  group,
  me,
  workspace,
  focused,
  closable,
  titles,
  setTitle,
  change,
  failed,
}: {
  group: GroupNode
  me: Me
  workspace: Workspace
  focused: boolean
  closable: boolean
  titles: Titles
  setTitle: (tab: string, path: string, title: string) => void
  change: ChangeFn
  failed: ReactNode
}) {
  const t = useT()
  const active = group.tabs.find((x) => x.id === group.active) ?? group.tabs[0]
  const route = parseRoute(active.path)
  const home = `/w/${workspace.id}/home`
  const titleOf = (id: string, path: string) => {
    const reported = titles[id]
    if (reported && reported.path === path) return reported.title
    const at = parseRoute(path)
    return at ? t(SECTION_LABEL[at.section]) : ''
  }
  const place = useMemo<Place>(
    () => ({
      id: active.id,
      path: active.path,
      navigate: (path, opts) =>
        focused ? navigate(path, opts) : askPlace(active.id, () => change((l) => navigateTab(l, active.id, path))),
      setTitle: (title) => {
        if (title !== null) setTitle(active.id, active.path, title)
      },
    }),
    [active.id, active.path, focused, change, setTitle],
  )

  return (
    <section
      className="desk-pane"
      data-place={active.id}
      data-focused={focused}
      aria-label={titleOf(active.id, active.path)}
      tabIndex={-1}
      onPointerDownCapture={() => change((l) => focusGroup(l, group.group))}
      onFocusCapture={() => change((l) => focusGroup(l, group.group))}
    >
      <TabStrip
        group={group.group}
        tabs={group.tabs.map((x) => ({ id: x.id, title: titleOf(x.id, x.path) }))}
        active={active.id}
        closable={closable}
        onActivate={(id) => id !== active.id && askPlace(active.id, () => change((l) => activate(l, id)))}
        onClose={(id) =>
          (group.tabs.length === 1 ? beforeLeaving : (go: () => void) => askPlace(id, go))(() =>
            change((l) => closeTab(l, id)),
          )
        }
        onNew={() => askPlace(active.id, () => change((l) => openTab(l, group.group, home)))}
        onSplit={(side: Side) => beforeLeaving(() => change((l) => splitGroup(l, group.group, side, home)))}
      />
      <div className="desk-panel" role="tabpanel" id={panelDomId(group.group)} aria-labelledby={tabDomId(active.id)}>
        <PlaceContext.Provider value={place}>
          <ErrorBoundary fallback={failed} resetKey={active.path}>
            {route ? (
              <SectionPage key={`${active.id}/${route.section}`} route={route} me={me} workspace={workspace} />
            ) : (
              failed
            )}
          </ErrorBoundary>
        </PlaceContext.Provider>
      </div>
    </section>
  )
}

import { type ReactNode, useCallback, useEffect, useRef, useState } from 'react'
import type { Me, Workspace } from '../api'
import { navigate, parseRoute, setFocusedPlace } from '../router'
import { activate, findTabByPath, focusedTab, isGroup, type LayoutNode, navigateTab, tabCount } from './layout'
import { type ChangeFn, Pane, type Titles } from './Pane'
import { useDesk } from './useDesk'
import './desk.css'

type Context = {
  me: Me
  workspace: Workspace
  focus: string
  closable: boolean
  titles: Titles
  setTitle: (tab: string, path: string, title: string) => void
  change: ChangeFn
  failed: ReactNode
}

const keyOf = (n: LayoutNode): string => (isGroup(n) ? n.group : keyOf(n.children[0]))

function NodeView({ node, ctx }: { node: LayoutNode; ctx: Context }) {
  if (isGroup(node)) {
    return (
      <Pane
        group={node}
        me={ctx.me}
        workspace={ctx.workspace}
        focused={ctx.focus === node.group}
        closable={ctx.closable}
        titles={ctx.titles}
        setTitle={ctx.setTitle}
        change={ctx.change}
        failed={ctx.failed}
      />
    )
  }
  return (
    <div className="desk-split" data-axis={node.split}>
      {node.children.map((child, i) => (
        <div key={keyOf(child)} className="desk-cell" style={{ flexGrow: node.sizes[i] }}>
          <NodeView node={child} ctx={ctx} />
        </div>
      ))}
    </div>
  )
}

export function Desk({
  me,
  workspace,
  path,
  failed,
}: {
  me: Me
  workspace: Workspace
  path: string
  failed: ReactNode
}) {
  const { layout, loaded, change } = useDesk(workspace.id, path)
  const [titles, setTitles] = useState<Titles>({})
  const setTitle = useCallback(
    (tab: string, at: string, title: string) => setTitles((all) => ({ ...all, [tab]: { path: at, title } })),
    [],
  )
  const [landedOnRoute] = useState(() => parseRoute(window.location.pathname) !== null)
  const focused = focusedTab(layout)
  const focusedId = focused?.id ?? null
  const focusedPath = focused?.path ?? path
  const seen = useRef<{ url: string | null; focus: string | null; loaded: boolean }>({
    url: null,
    focus: null,
    loaded: false,
  })

  useEffect(() => {
    setFocusedPlace(focusedId)
    return () => setFocusedPlace(null)
  }, [focusedId])

  useEffect(() => {
    const s = seen.current
    if (s.loaded !== loaded) {
      s.loaded = loaded
      s.url = landedOnRoute ? null : path
    }
    if (path !== s.url) {
      s.url = path
      s.focus = focusedPath
      if (path === focusedPath || focusedId === null) return
      const found = findTabByPath(layout, path)
      change((l) => (found ? activate(l, found.tabId) : navigateTab(l, focusedId, path)))
      return
    }
    if (focusedPath !== s.focus) {
      s.focus = focusedPath
      if (focusedPath !== path) navigate(focusedPath, { replace: true })
    }
  }, [path, focusedPath, focusedId, loaded, layout, change, landedOnRoute])

  if (!layout.root) return null
  const ctx: Context = {
    me,
    workspace,
    focus: layout.focus,
    closable: tabCount(layout) > 1,
    titles,
    setTitle,
    change,
    failed,
  }
  return <NodeView node={layout.root} ctx={ctx} />
}

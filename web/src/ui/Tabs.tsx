import { type KeyboardEvent, type ReactNode, useId, useRef } from 'react'
import { Link } from '../router'
import './Tabs.css'

export interface TabItem {
  id: string
  label: ReactNode
  count?: ReactNode
  href?: string
}

export interface TabsProps {
  label: string
  items: readonly TabItem[]
  value: string
  onChange?: (id: string) => void
  id?: string
}

const tabId = (base: string, item: string) => `${base}-tab-${item}`
const panelId = (base: string, item: string) => `${base}-panel-${item}`

export function tabPanelProps(base: string, item: string) {
  return { role: 'tabpanel', id: panelId(base, item), 'aria-labelledby': tabId(base, item), tabIndex: 0 } as const
}

function moveTo(key: string, from: number, count: number, rtl: boolean): number | null {
  const next = rtl ? 'ArrowLeft' : 'ArrowRight'
  const prev = rtl ? 'ArrowRight' : 'ArrowLeft'
  if (key === next) return (from + 1) % count
  if (key === prev) return (from - 1 + count) % count
  if (key === 'Home') return 0
  if (key === 'End') return count - 1
  return null
}

const isRtl = (el: Element) => getComputedStyle(el).direction === 'rtl'

export function Tabs({ label, items, value, onChange, id }: TabsProps) {
  const list = useRef<HTMLDivElement>(null)
  const links = items.every((i) => i.href !== undefined)
  if (import.meta.env.DEV && !links && items.some((i) => i.href !== undefined)) {
    console.error('Tabs: every item has an href (links) or none has (buttons); these are drawn as buttons.')
  }
  const auto = useId()
  const base = id ?? auto

  const keyDown = (e: KeyboardEvent) => {
    const el = list.current
    if (!el) return
    const tabs = [...el.querySelectorAll<HTMLElement>('.ui-tab')]
    const from = tabs.indexOf(document.activeElement as HTMLElement)
    if (from < 0) return
    const to = moveTo(e.key, from, tabs.length, isRtl(el))
    if (to === null) return
    e.preventDefault()
    tabs[to].focus()
    if (!links) onChange?.(items[to].id)
  }

  const content = (item: TabItem) => (
    <>
      <span className="ui-tab-label">{item.label}</span>
      {item.count !== undefined && <span className="ui-tab-count">{item.count}</span>}
    </>
  )

  if (links) {
    return (
      <nav className="ui-tabs" aria-label={label}>
        <div ref={list} className="ui-tabs-list">
          {items.map((item) => (
            <Link
              key={item.id}
              href={item.href as string}
              className="ui-tab"
              aria-current={item.id === value ? 'page' : undefined}
              onKeyDown={keyDown}
            >
              {content(item)}
            </Link>
          ))}
        </div>
      </nav>
    )
  }

  return (
    <div className="ui-tabs">
      <div ref={list} className="ui-tabs-list" role="tablist" aria-label={label} onKeyDown={keyDown}>
        {items.map((item) => {
          const selected = item.id === value
          return (
            <button
              key={item.id}
              type="button"
              role="tab"
              id={tabId(base, item.id)}
              aria-controls={panelId(base, item.id)}
              aria-selected={selected}
              tabIndex={selected ? 0 : -1}
              className="ui-tab"
              onClick={() => onChange?.(item.id)}
            >
              {content(item)}
            </button>
          )
        })}
      </div>
    </div>
  )
}

import {
  createContext,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useContext,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from 'react'
import { useT } from '../i18n'
import { Link } from '../router'
import { Button, type ButtonProps } from './Button'
import { Icon, type IconName } from './Icon'
import { IconButton } from './IconButton'
import { place, scrolledAway } from './place'
import './VisuallyHidden.css'
import './Menu.css'

interface MenuState {
  open: boolean
  start: 'first' | 'last'
  openMenu: (start: 'first' | 'last') => void
  closeMenu: (refocus: boolean) => void
  button: RefObject<HTMLButtonElement | null>
  list: RefObject<HTMLDivElement | null>
  buttonId: string
  listId: string
  align: 'start' | 'end'
}

const MenuContext = createContext<MenuState | null>(null)

function useMenu(part: string): MenuState {
  const menu = useContext(MenuContext)
  if (!menu) throw new Error(`${part} must be inside a Menu`)
  return menu
}

export interface MenuProps {
  children: ReactNode
  align?: 'start' | 'end'
}

export function Menu({ children, align = 'end' }: MenuProps) {
  const [open, setOpen] = useState(false)
  const [start, setStart] = useState<'first' | 'last'>('first')
  const button = useRef<HTMLButtonElement>(null)
  const list = useRef<HTMLDivElement>(null)
  const id = useId()

  const openMenu = useCallback((from: 'first' | 'last') => {
    setStart(from)
    setOpen(true)
  }, [])
  const closeMenu = useCallback((refocus: boolean) => {
    setOpen(false)
    if (refocus) button.current?.focus()
  }, [])

  const state = useMemo(
    () => ({
      open,
      start,
      openMenu,
      closeMenu,
      button,
      list,
      buttonId: `${id}-button`,
      listId: `${id}-list`,
      align,
    }),
    [open, start, openMenu, closeMenu, id, align],
  )
  return (
    <MenuContext.Provider value={state}>
      <span className="ui-menu">{children}</span>
    </MenuContext.Provider>
  )
}

export interface MenuButtonProps extends Omit<ButtonProps, 'onClick' | 'children'> {
  label: string
  icon?: IconName
  children?: ReactNode
  indicator?: IconName | null
}

export function MenuButton({
  label,
  icon = 'more',
  children,
  indicator = 'chevron-down',
  variant = 'ghost',
  size,
  ...rest
}: MenuButtonProps) {
  const menu = useMenu('MenuButton')
  const t = useT()
  const wasOpen = useRef(false)

  const common = {
    ref: menu.button,
    id: menu.buttonId,
    'aria-haspopup': 'menu' as const,
    'aria-expanded': menu.open,
    'aria-controls': menu.open ? menu.listId : undefined,
    onPointerDown: () => {
      wasOpen.current = menu.open
    },
    onClick: () => {
      const reopen = !wasOpen.current && !menu.open
      wasOpen.current = false
      if (reopen) menu.openMenu('first')
      else menu.closeMenu(false)
    },
    onKeyDown: (e: KeyboardEvent<HTMLButtonElement>) => {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault()
        menu.openMenu(e.key === 'ArrowDown' ? 'first' : 'last')
      }
    },
  }

  if (children === undefined) {
    return <IconButton {...rest} {...common} icon={icon} label={label} size={size} />
  }
  return (
    <Button
      {...rest}
      {...common}
      variant={variant}
      size={size}
      aria-labelledby={`${menu.buttonId}-label ${menu.buttonId}-value`}
    >
      <span className="ui-menu-button-text">
        <span id={`${menu.buttonId}-label`} className="ui-visually-hidden">
          {t('ui.menuButtonName', { label })}
        </span>
        <span id={`${menu.buttonId}-value`} className="ui-menu-button-value">
          {children}
        </span>
        {indicator && <Icon name={indicator} size={16} />}
      </span>
    </Button>
  )
}

const ITEM = '[role="menuitem"], [role="menuitemradio"]'

export function MenuList({ children, label }: { children: ReactNode; label?: string }) {
  const menu = useMenu('MenuList')
  const { open, start, list, button, closeMenu, align } = menu
  const typed = useRef({ text: '', at: 0 })

  useLayoutEffect(() => {
    const el = list.current
    if (!el || !open) return
    el.showPopover()
    const position = () => {
      const anchor = button.current
      if (!anchor) return
      const a = anchor.getBoundingClientRect()
      const scrolled = el.scrollTop
      el.style.maxBlockSize = ''
      const box = el.getBoundingClientRect()
      const p = place(
        { top: a.top, left: a.left, width: a.width, height: a.height },
        { width: box.width, height: box.height },
        { width: document.documentElement.clientWidth, height: window.innerHeight },
        { dir: getComputedStyle(anchor).direction === 'rtl' ? 'rtl' : 'ltr', align, gap: 4, margin: 8 },
      )
      el.style.top = `${p.top}px`
      el.style.left = `${p.left}px`
      el.style.maxBlockSize = `${p.maxHeight}px`
      el.scrollTop = scrolled
      el.dataset.side = p.side
    }
    position()
    const items = el.querySelectorAll<HTMLElement>(ITEM)
    ;(items[start === 'first' ? 0 : items.length - 1] ?? el).focus()

    let frame = 0
    const follow = () => {
      if (button.current && scrolledAway(button.current)) closeMenu(false)
      else position()
    }
    const later = () => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(position)
    }
    const scrolled = (e: Event) => {
      if (e.target instanceof Node && el.contains(e.target)) return
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(follow)
    }
    window.addEventListener('resize', later)
    window.addEventListener('scroll', scrolled, true)
    return () => {
      cancelAnimationFrame(frame)
      window.removeEventListener('resize', later)
      window.removeEventListener('scroll', scrolled, true)
      el.hidePopover()
    }
  }, [open, start, list, button, align, closeMenu])

  useEffect(() => {
    if (!open) return
    const outside = (e: PointerEvent) => {
      const target = e.target as Node
      if (list.current?.contains(target) || button.current?.contains(target)) return
      closeMenu(false)
    }
    document.addEventListener('pointerdown', outside, true)
    return () => document.removeEventListener('pointerdown', outside, true)
  }, [open, list, button, closeMenu])

  const keyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const items = [...(list.current?.querySelectorAll<HTMLElement>(ITEM) ?? [])]
    const at = items.indexOf(document.activeElement as HTMLElement)
    const go = (i: number) => {
      e.preventDefault()
      items[(i + items.length) % items.length]?.focus()
    }
    switch (e.key) {
      case 'ArrowDown':
        return go(at + 1)
      case 'ArrowUp':
        return go(at - 1)
      case 'Home':
        return go(0)
      case 'End':
        return go(items.length - 1)
      case 'Escape':
        if (e.defaultPrevented) return
        e.preventDefault()
        e.stopPropagation()
        return closeMenu(true)
      case 'Tab':
        return closeMenu(false)
    }
    if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey && e.key !== ' ') {
      const now = Date.now()
      const typedSoFar = (now - typed.current.at < 600 ? typed.current.text : '') + e.key.toLocaleLowerCase()
      typed.current = { text: typedSoFar, at: now }
      const repeated = [...typedSoFar].every((c) => c === typedSoFar[0])
      const text = repeated ? typedSoFar[0] : typedSoFar
      const from = text.length > 1 ? at : at + 1
      for (let n = 0; n < items.length; n++) {
        const item = items[(from + n + items.length) % items.length]
        if (item.textContent?.trim().toLocaleLowerCase().startsWith(text)) return go(items.indexOf(item))
      }
    }
  }

  return (
    <div
      ref={list}
      id={menu.listId}
      className="ui-menu-list"
      role="menu"
      aria-labelledby={label ? undefined : menu.buttonId}
      aria-label={label}
      popover="manual"
      tabIndex={-1}
      onKeyDown={keyDown}
    >
      {open && children}
    </div>
  )
}

export interface MenuItemProps {
  children: ReactNode
  onSelect: () => void
  icon?: IconName
  tone?: 'danger'
  disabled?: boolean
  checked?: boolean
}

export function MenuItem({ children, onSelect, icon, tone, disabled, checked }: MenuItemProps) {
  const menu = useMenu('MenuItem')
  const common = {
    type: 'button' as const,
    tabIndex: -1,
    className: 'ui-menu-item',
    'data-tone': tone,
    'aria-disabled': disabled || undefined,
    onClick: () => {
      if (disabled) return
      menu.closeMenu(true)
      onSelect()
    },
  }
  const content = (
    <>
      {icon && <Icon name={icon} />}
      <span className="ui-menu-item-label">{children}</span>
    </>
  )
  if (checked === undefined) {
    return (
      <button role="menuitem" {...common}>
        {content}
      </button>
    )
  }
  return (
    <button role="menuitemradio" aria-checked={checked} {...common}>
      {content}
      <Icon name="check" size={16} className="ui-menu-item-check" />
    </button>
  )
}

export function MenuLink({ href, children, icon }: { href: string; children: ReactNode; icon?: IconName }) {
  const menu = useMenu('MenuLink')
  return (
    <Link
      href={href}
      role="menuitem"
      tabIndex={-1}
      className="ui-menu-item"
      onClick={() => menu.closeMenu(false)}
      onKeyDown={(e) => {
        if (e.key === ' ') {
          e.preventDefault()
          e.currentTarget.click()
        }
      }}
    >
      {icon && <Icon name={icon} />}
      <span className="ui-menu-item-label">{children}</span>
    </Link>
  )
}

export function MenuGroup({ label, children }: { label: string; children: ReactNode }) {
  const id = useId()
  return (
    <fieldset aria-labelledby={id} className="ui-menu-group">
      <div id={id} role="presentation" className="ui-menu-group-label">
        {label}
      </div>
      {children}
    </fieldset>
  )
}

export function MenuSeparator() {
  return <hr className="ui-menu-separator" />
}

export function MoreMenu({ children, label, align }: { children: ReactNode; label?: string; align?: 'start' | 'end' }) {
  const t = useT()
  return (
    <Menu align={align}>
      <MenuButton label={label ?? t('ui.moreActions')} />
      <MenuList>{children}</MenuList>
    </Menu>
  )
}

// A menu of actions behind a button: the "more" menu of a row, a card, a document.
//
//   <Menu>
//     <MenuButton label={moreLabel} />
//     <MenuList>
//       <MenuItem icon="edit" onSelect={rename}>{renameLabel}</MenuItem>
//       <MenuSeparator />
//       <MenuItem tone="danger" onSelect={remove}>{deleteLabel}</MenuItem>
//     </MenuList>
//   </Menu>
//
// or <MoreMenu> with the items, for the usual icon button. The list is a popover (the browser's
// top layer, so no scrolling or clipped ancestor cuts it, and it is above a drawer), placed by
// place.ts inside the window, aligned to the button's end in the writing direction.
//
// Keyboard (the WAI-ARIA menu button): Enter, Space or ArrowDown open it on the first item,
// ArrowUp on the last; ArrowDown and ArrowUp move (round), Home and End, a letter moves to the
// next item that starts with it; Enter or Space chooses; Escape closes it and Tab leaves it. A
// click outside closes it. Closing it by Escape or by choosing gives the focus back to the
// button.

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
  // Where the focus goes when the list opens.
  start: 'first' | 'last'
  openMenu: (start: 'first' | 'last') => void
  // Close; give the focus back to the button when `refocus`.
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
  // Which of the button's edges the list lines up with, in the writing direction.
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
  // The button's name. With no `children`, it is an icon button and this is its tooltip.
  label: string
  icon?: IconName
  // Text on the button, which then shows a chevron after it.
  children?: ReactNode
  // The sign after the text: a chevron down (the default), another icon (`chevron-updown` for a
  // switcher), or none (`null`), for a button whose text already says what it opens.
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
  // A press on the button while the list is open closes it (the click outside does), and the
  // click that follows must not open it again.
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
    // Its name is what it chooses, then what it shows: "Sort by: Newest first". The words you see
    // stay in the name (a voice command says them), and the label says what they are.
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

  // Show it in the top layer, place it, and focus the first or the last item.
  useLayoutEffect(() => {
    const el = list.current
    if (!el || !open) return
    el.showPopover()
    const position = () => {
      const anchor = button.current
      if (!anchor) return
      const a = anchor.getBoundingClientRect()
      el.style.maxBlockSize = ''
      const box = el.getBoundingClientRect()
      const p = place(
        { top: a.top, left: a.left, width: a.width, height: a.height },
        { width: box.width, height: box.height },
        { width: document.documentElement.clientWidth, height: window.innerHeight },
        { dir: getComputedStyle(anchor).direction === 'rtl' ? 'rtl' : 'ltr', align, gap: 4, margin: 8 },
      )
      // The window's own coordinates (place() has taken the writing direction into account).
      el.style.top = `${p.top}px`
      el.style.left = `${p.left}px`
      el.style.maxBlockSize = `${p.maxHeight}px`
      el.dataset.side = p.side
    }
    position()
    const items = el.querySelectorAll<HTMLElement>(ITEM)
    items[start === 'first' ? 0 : items.length - 1]?.focus()

    // Keep it beside the button while the page scrolls or the window changes size.
    let frame = 0
    // A button scrolled out of sight takes its menu with it.
    const follow = () => {
      if (button.current && scrolledAway(button.current)) closeMenu(false)
      else position()
    }
    const later = () => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(position)
    }
    const scrolled = () => {
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

  // A press outside the list and the button closes it, and the focus stays where it went.
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
        // A tooltip that could be seen took this one (Tooltip.tsx): the next is the menu's.
        if (e.defaultPrevented) return
        // Handled here: a drawer or a dialog under the menu stays open.
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
      // One letter again and again ("r r r") goes round the items that start with it; letters
      // that spell more ("re") look for an item that starts with them all.
      const repeated = [...typedSoFar].every((c) => c === typedSoFar[0])
      const text = repeated ? typedSoFar[0] : typedSoFar
      // From the item after this one (the same one when the letters go on), round the list.
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
  // A destructive action: in the danger colour, and last, after a separator.
  tone?: 'danger'
  disabled?: boolean
  // One choice of several (a theme, a language): a radio item, checked or not, with a check mark
  // on the one that is. Put the choices in a MenuGroup that names them.
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

// An item that goes somewhere: a real link, so it opens in a new tab or is copied as any link is,
// drawn as an item. Enter follows it, as it does any link, and so does Space, as for the other items.
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

// A named group of items: the choices of one setting, with its name above them.
export function MenuGroup({ label, children }: { label: string; children: ReactNode }) {
  const id = useId()
  return (
    // biome-ignore lint/a11y/useSemanticElements: a group of a menu's items is role="group" inside role="menu"; a <fieldset> is a form's.
    <div role="group" aria-labelledby={id} className="ui-menu-group">
      <div id={id} role="presentation" className="ui-menu-group-label">
        {label}
      </div>
      {children}
    </div>
  )
}

export function MenuSeparator() {
  // An <hr> in a menu is its separator.
  return <hr className="ui-menu-separator" />
}

// The usual "more" menu: an icon button named "More actions" (or `label`) and the items.
export function MoreMenu({ children, label, align }: { children: ReactNode; label?: string; align?: 'start' | 'end' }) {
  const t = useT()
  return (
    <Menu align={align}>
      <MenuButton label={label ?? t('ui.moreActions')} />
      <MenuList>{children}</MenuList>
    </Menu>
  )
}

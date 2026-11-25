// The slim bar a phone has instead of the sidebar: the menu button that opens the sidebar as a
// drawer, the logo's mark, and the name of the section the person is in. Wider screens don't draw
// it (Shell.css): the sidebar is always there, and the page owns its title.
//
// It never overflows, from 320px up: the button and the mark keep their size, and the section's
// name gives way, cut with an ellipsis.

import type { Ref } from 'react'
import { BrandLogo } from '../BrandLogo'
import type { T } from '../i18n'
import { IconButton } from '../ui/IconButton'
import { BRAND, SIDEBAR_ID } from './nav'

export interface PhoneBarProps {
  drawer: boolean
  onToggle: () => void
  menuButton: Ref<HTMLButtonElement>
  section: string
  t: T
}

export function PhoneBar({ drawer, onToggle, menuButton, section, t }: PhoneBarProps) {
  return (
    <header className="ui-shell-bar">
      <IconButton
        ref={menuButton}
        icon="menu"
        className="ui-shell-menu-button"
        label={drawer ? t('shell.closeMenu') : t('shell.openMenu')}
        aria-expanded={drawer}
        aria-controls={SIDEBAR_ID}
        onClick={onToggle}
      />
      <span className="ui-shell-bar-mark">
        <BrandLogo name={BRAND} />
      </span>
      <span className="ui-shell-bar-title">{section}</span>
    </header>
  )
}

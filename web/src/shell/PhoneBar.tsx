import type { Ref } from 'react'
import { BrandLogo } from '../BrandLogo'
import type { T } from '../i18n'
import { IconButton } from '../ui/IconButton'
import { BRAND, SIDEBAR_ID } from './nav'

export interface PhoneBarProps {
  drawer: boolean
  onToggle: () => void
  menuButton: Ref<HTMLButtonElement>
  t: T
}

export function PhoneBar({ drawer, onToggle, menuButton, t }: PhoneBarProps) {
  return (
    <header className="ui-shell-bar" aria-label={t('shell.phoneBar')}>
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
    </header>
  )
}

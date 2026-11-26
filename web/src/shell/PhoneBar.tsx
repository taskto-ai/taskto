// The slim bar a phone has instead of the sidebar: the menu button that opens the sidebar as a
// drawer, and the logo's mark. The section's name is not here: the page's own title, just under the
// bar, is the one place it appears. Wider screens don't draw the bar (Shell.css): the sidebar is
// always there. It is a landmark with a name of its own, so a screen reader can still find it.
//
// It never overflows, from 320px up: the button and the mark keep their size.

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
    // A <header> outside any section is the page's banner landmark, whose name is what a screen
    // reader lists it by; Biome's rule takes a header for a plain element.
    // biome-ignore lint/a11y/useAriaPropsSupportedByRole: the banner landmark takes a name
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

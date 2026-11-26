// What the shell's parts share: the sections in the sidebar, the product's name, the ids that tie
// the menu button to the sidebar and the skip link to the page.

import type { Key } from '../i18n'
import type { Section } from '../router'
import type { IconName } from '../ui/Icon'

export type NavSection = Exclude<Section, 'notifications' | 'settings'>

// The platform's pages, in the sidebar's order. Settings is at the sidebar's foot, the inbox after
// these, and the pinned apps after the inbox.
export const NAV: { section: NavSection; label: Key; icon: IconName }[] = [
  { section: 'home', label: 'shell.sections.home', icon: 'home' },
  { section: 'conversations', label: 'shell.sections.conversations', icon: 'conversations' },
  { section: 'knowledge', label: 'shell.sections.knowledge', icon: 'knowledge' },
  { section: 'boards', label: 'shell.sections.boards', icon: 'boards' },
  { section: 'apps', label: 'shell.sections.apps', icon: 'apps' },
]

// Each section's name, for the phone bar.
export const SECTION_LABEL: Record<Section, Key> = {
  home: 'shell.sections.home',
  conversations: 'shell.sections.conversations',
  knowledge: 'shell.sections.knowledge',
  boards: 'shell.sections.boards',
  apps: 'shell.sections.apps',
  settings: 'shell.sections.settings',
  notifications: 'notifications.title',
}

// The product's name, the same in every language.
export const BRAND = 'taskto.ai'

export const SIDEBAR_ID = 'ui-shell-sidebar'
export const MAIN_ID = 'ui-shell-main'

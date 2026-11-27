import type { Key } from '../i18n'
import type { Section } from '../router'
import type { IconName } from '../ui/Icon'

export type NavSection = Exclude<Section, 'notifications' | 'settings'>

export const NAV: { section: NavSection; label: Key; icon: IconName }[] = [
  { section: 'home', label: 'shell.sections.home', icon: 'home' },
  { section: 'conversations', label: 'shell.sections.conversations', icon: 'conversations' },
  { section: 'knowledge', label: 'shell.sections.knowledge', icon: 'knowledge' },
  { section: 'boards', label: 'shell.sections.boards', icon: 'boards' },
  { section: 'apps', label: 'shell.sections.apps', icon: 'apps' },
]

export const SECTION_LABEL: Record<Section, Key> = {
  home: 'shell.sections.home',
  conversations: 'shell.sections.conversations',
  knowledge: 'shell.sections.knowledge',
  boards: 'shell.sections.boards',
  apps: 'shell.sections.apps',
  settings: 'shell.sections.settings',
  notifications: 'notifications.title',
}

export const BRAND = 'taskto.ai'

export const SIDEBAR_ID = 'ui-shell-sidebar'
export const MAIN_ID = 'ui-shell-main'

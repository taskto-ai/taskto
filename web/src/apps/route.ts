// A launched app's address, apart from the Apps page so the chat links to an app without
// loading the A2UI renderer.

import { routePath } from '../router'

// A launched app's route item: "{space}-{app}", or "{space}-{app}-{screen}" to open it on one of its
// screens (what a pin that names one does).
export function appItem(space: number, app: number, screen?: string): string {
  return `${space}-${app}${screen ? `-${screen}` : ''}`
}

export function parseItem(item: string | undefined): { space: number; app: number; screen?: string } | null {
  const m = item ? /^(\d+)-(\d+)(?:-([a-z][a-z0-9_]{0,39}))?$/.exec(item) : null
  return m ? { space: Number(m[1]), app: Number(m[2]), ...(m[3] ? { screen: m[3] } : {}) } : null
}

// The Apps page with the app launched.
export function appPath(workspace: number, space: number, app: number, screen?: string): string {
  return routePath({ workspace, section: 'apps', item: appItem(space, app, screen) })
}

// The builder's addresses, under the Apps page: "build" starts a new app in a new conversation,
// "build-{conversation}" builds in a conversation (the one that is already there), and
// "edit-{space}-{app}" asks the agent to change an app's draft.
export type BuilderRoute =
  | { kind: 'new' }
  | { kind: 'conversation'; conversation: number }
  | { kind: 'edit'; space: number; app: number }

export function parseBuilder(item: string | undefined): BuilderRoute | null {
  if (item === 'build') return { kind: 'new' }
  const conversation = item ? /^build-(\d+)$/.exec(item) : null
  if (conversation) return { kind: 'conversation', conversation: Number(conversation[1]) }
  const edit = item ? /^edit-(\d+)-(\d+)$/.exec(item) : null
  return edit ? { kind: 'edit', space: Number(edit[1]), app: Number(edit[2]) } : null
}

export function builderPath(workspace: number, to: BuilderRoute = { kind: 'new' }): string {
  const item =
    to.kind === 'new' ? 'build' : to.kind === 'conversation' ? `build-${to.conversation}` : `edit-${to.space}-${to.app}`
  return routePath({ workspace, section: 'apps', item })
}

// The templates' addresses, under the Apps page: "templates" is the list to start from, and
// "installed-{space}-{app}" the draft a template or a copy made, to try and publish.
export function templatesPath(workspace: number): string {
  return routePath({ workspace, section: 'apps', item: 'templates' })
}

export function isTemplates(item: string | undefined): boolean {
  return item === 'templates'
}

export function installedPath(workspace: number, space: number, app: number): string {
  return routePath({ workspace, section: 'apps', item: `installed-${space}-${app}` })
}

export function parseInstalled(item: string | undefined): { space: number; app: number } | null {
  const m = item ? /^installed-(\d+)-(\d+)$/.exec(item) : null
  return m ? { space: Number(m[1]), app: Number(m[2]) } : null
}

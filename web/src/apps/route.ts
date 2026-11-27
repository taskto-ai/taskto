import { routePath } from '../router'

export function appItem(space: number, app: number, screen?: string): string {
  return `${space}-${app}${screen ? `-${screen}` : ''}`
}

export function parseItem(item: string | undefined): { space: number; app: number; screen?: string } | null {
  const m = item ? /^(\d+)-(\d+)(?:-([a-z][a-z0-9_]{0,39}))?$/.exec(item) : null
  return m ? { space: Number(m[1]), app: Number(m[2]), ...(m[3] ? { screen: m[3] } : {}) } : null
}

export function appPath(workspace: number, space: number, app: number, screen?: string): string {
  return routePath({ workspace, section: 'apps', item: appItem(space, app, screen) })
}

export function runPath(workspace: number, space: number, app: number, screen?: string): string {
  return `/w/${workspace}/run/${appItem(space, app, screen)}`
}

export function parseRunPath(path: string): { workspace: number; space: number; app: number; screen?: string } | null {
  const m = /^\/w\/(\d+)\/run\/([^/]+)\/?$/.exec(path)
  const item = parseItem(m?.[2])
  return m && item ? { workspace: Number(m[1]), ...item } : null
}

export function runWindowName(space: number, app: number): string {
  return `taskto-app-${space}-${app}`
}

export const RUN_WINDOW_FEATURES = 'popup,width=480,height=800'

const runWindows = new Map<string, Window>()

export function openRunWindow(workspace: number, space: number, app: number, screen?: string): void {
  const name = runWindowName(space, app)
  const held = runWindows.get(name)
  const popup = held && !held.closed ? held : window.open('', name, RUN_WINDOW_FEATURES)
  if (!popup) {
    runWindows.delete(name)
    return
  }
  popup.opener = null
  runWindows.set(name, popup)
  popup.location.href = runPath(workspace, space, app, screen)
  popup.focus()
}

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

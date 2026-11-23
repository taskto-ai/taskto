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

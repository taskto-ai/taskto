// A launched app's address, apart from the Apps page so the chat links to an app without
// loading the A2UI renderer.

import { routePath } from '../router'

// A launched app's route item: "{space}-{app}".
export function appItem(space: number, app: number): string {
  return `${space}-${app}`
}

export function parseItem(item: string | undefined): { space: number; app: number } | null {
  const m = item ? /^(\d+)-(\d+)$/.exec(item) : null
  return m ? { space: Number(m[1]), app: Number(m[2]) } : null
}

// The Apps page with the app launched.
export function appPath(workspace: number, space: number, app: number): string {
  return routePath({ workspace, section: 'apps', item: appItem(space, app) })
}

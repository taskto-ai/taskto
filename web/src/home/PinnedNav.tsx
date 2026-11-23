// The apps a person pinned (and those a shared space pins for its members), listed in the
// navigation in a section of their own, after the platform's pages: a pin is only ever one more link
// to launch an app, whatever it says, so it can't stand in for or hide a platform page. Its label is
// drawn as plain text. Unavailable pins (the app is offline or was never published) aren't listed.
//
// Loading and drawing them can't take the navigation down: a failed load shows nothing, and the
// shell wraps this in an error boundary for an answer that is malformed.

import { api, type HomeView, ws } from '../api'
import { appPath } from '../apps/route'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Link } from '../router'
import { Icon } from '../shell/Icon'
import { useHomeVersion } from './store'

export function PinnedNav({ workspace, path }: { workspace: number; path: string }) {
  const version = useHomeVersion()
  const home = useLoad(() => api<HomeView>(`${ws(workspace)}/home`), [workspace, version])
  const t = useT()
  const pins = (home.data?.pins ?? []).filter((p) => p.state === 'ready')
  if (pins.length === 0) return null
  return (
    <section className="tt-pins" aria-labelledby="tt-pins-title">
      <h2 id="tt-pins-title" className="tt-pins-title">
        {t('shell.pinned')}
      </h2>
      <ul>
        {pins.map((p) => {
          const href = appPath(workspace, p.space_id, p.app_id, p.screen)
          return (
            <li key={`${p.source}:${p.id ?? p.default_id}`}>
              <Link href={href} className="tt-nav-link" aria-current={href === path ? 'page' : undefined}>
                <Icon name="pin" />
                <span className="tt-pin-label">{p.label || p.app_name}</span>
              </Link>
            </li>
          )
        })}
      </ul>
    </section>
  )
}
